// Package cas implements content-addressed storage: content-defined chunking,
// BLAKE3 addressing, zstd compression, and reassembly.
//
// The point is dedup. Two users uploading the same file converge on the same
// chunk rows without either knowing the other exists; a 4 GB video re-uploaded
// after a one-byte metadata edit stores one changed chunk rather than 4 GB.
//
// Content-DEFINED chunking, not fixed-size blocks, is what makes that true.
// Fixed blocks dedup nothing after a one-byte prepend, because every subsequent
// boundary shifts. FastCDC picks boundaries from the content itself, so an
// insertion perturbs one chunk and leaves the rest identical.
package cas

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/tigerwill90/fastcdc"
	"github.com/zeebo/blake3"
)

// Chunking parameters. See docs/status.md § Phase 2 for the reasoning; the
// short version is that below MinSize a chunk's database row costs more than
// the chunk saves, and above MaxSize a single edit invalidates too much.
const (
	MinChunkSize = 2 << 10  // 2 KiB
	AvgChunkSize = 16 << 10 // 16 KiB
	MaxChunkSize = 64 << 10 // 64 KiB

	// WholeFileThreshold is the size below which a file is not chunked at all.
	// A manifest row plus a chunk row to describe 900 bytes is pure overhead,
	// and nothing that small dedups usefully.
	WholeFileThreshold = MinChunkSize
)

// Compression identifiers, stored per chunk.
const (
	CompressionNone = "none"
	CompressionZstd = "zstd"
)

// Chunk is one content-defined piece of a file.
type Chunk struct {
	// Hash is BLAKE3-256 of the PLAINTEXT. Addressing by plaintext rather than
	// by compressed output matters: zstd's output can change with a library
	// version, and addressing by it would silently stop deduplicating after a
	// dependency upgrade.
	Hash [32]byte

	// Size is the plaintext length; Stored is what actually goes to disk.
	Size   int
	Stored []byte

	Compression string

	// Offset is the chunk's byte position within the file.
	Offset int64
}

// ChunkFn receives each chunk in order. The Chunk and its Stored bytes are
// owned by the callee and remain valid after it returns.
type ChunkFn func(*Chunk) error

// Split divides r into content-defined chunks, compressing each where that
// helps, and returns BLAKE3 of the whole reassembled file.
//
// The callback shape is the library's, and it is a better fit than a pull-based
// iterator anyway: the caller writes each chunk to storage as it arrives rather
// than accumulating a slice, so memory stays flat regardless of file size.
//
// github.com/tigerwill90/fastcdc rather than jotfs/fastcdc-go. The latter
// mutates a PACKAGE-LEVEL gearing table inside NewChunker, which races against
// the table reads every other in-flight chunker performs in its inner loop —
// confirmed with the race detector, and not fixable from outside the library
// since guarding construction does nothing about concurrent readers. This one
// treats its table as the constant it is.
func Split(ctx context.Context, r io.Reader, fn ChunkFn) ([32]byte, error) {
	splitter, err := fastcdc.NewChunker(ctx,
		fastcdc.WithChunksSize(MinChunkSize, AvgChunkSize, MaxChunkSize))
	if err != nil {
		return [32]byte{}, fmt.Errorf("create chunker: %w", err)
	}

	// Level 3 (SpeedDefault) is the knee of the curve. Higher levels cost CPU
	// that a single-box deployment does not have spare while also hashing and
	// writing at the same time.
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		return [32]byte{}, fmt.Errorf("create zstd encoder: %w", err)
	}
	defer enc.Close()

	whole := blake3.New()

	emit := func(offset, length uint, plain []byte) error {
		// The library reuses its internal buffer, so `plain` is valid only
		// inside this callback. Everything derived from it is computed or
		// copied here.
		whole.Write(plain)

		ch := &Chunk{
			Hash:        blake3.Sum256(plain),
			Size:        len(plain),
			Offset:      int64(offset),
			Compression: CompressionNone,
		}

		// Keep compression only when it helps by a clear margin. A "compressed"
		// chunk that grew is strictly worse than the original, and paying
		// decompression on every read to save 2% is not worth it. Deciding per
		// chunk is how already-compressed content (JPEG, MP4, .zst) skips zstd
		// without needing a MIME lookup.
		//
		// EncodeAll allocates rather than reusing a buffer: the result outlives
		// this call, and handing it storage the next chunk would overwrite is
		// exactly the aliasing bug that produces one corrupt chunk in a
		// thousand.
		if compressed := enc.EncodeAll(plain, nil); len(compressed) < len(plain)*9/10 {
			ch.Stored = compressed
			ch.Compression = CompressionZstd
		} else {
			ch.Stored = bytes.Clone(plain)
		}

		_ = length // length == len(plain); the Chunk carries it as Size
		return fn(ch)
	}

	if err := splitter.Split(r, emit); err != nil {
		return [32]byte{}, fmt.Errorf("split: %w", err)
	}
	// Finalize emits the trailing partial chunk. Skipping it silently truncates
	// every file whose length is not a chunk boundary — which is nearly all of
	// them.
	if err := splitter.Finalize(emit); err != nil {
		return [32]byte{}, fmt.Errorf("finalize split: %w", err)
	}

	var out [32]byte
	copy(out[:], whole.Sum(nil))
	return out, nil
}

// --- decompression ----------------------------------------------------------

// decoder is shared: zstd decoders are safe for concurrent use, and allocating
// one per chunk read would dominate the cost of reading a small chunk.
var (
	decoderOnce sync.Once
	decoder     *zstd.Decoder
	decoderErr  error
)

func sharedDecoder() (*zstd.Decoder, error) {
	decoderOnce.Do(func() {
		decoder, decoderErr = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	})
	return decoder, decoderErr
}

// Decompress returns the plaintext of a stored chunk.
func Decompress(stored []byte, compression string, plainSize int) ([]byte, error) {
	switch compression {
	case CompressionNone:
		return stored, nil
	case CompressionZstd:
		dec, err := sharedDecoder()
		if err != nil {
			return nil, fmt.Errorf("zstd decoder: %w", err)
		}
		out, err := dec.DecodeAll(stored, make([]byte, 0, plainSize))
		if err != nil {
			return nil, fmt.Errorf("decompress chunk: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown compression %q", compression)
	}
}

// --- storage keys -----------------------------------------------------------

// StorageKey maps a chunk hash to its path in the blob store.
//
// Same two-level fan-out as whole-file blobs — ab/cd/<full hex> — so one
// directory listing stays comprehensible and the two storage formats live
// side by side without a second layout to reason about.
func StorageKey(hash [32]byte) string {
	h := hex.EncodeToString(hash[:])
	return h[0:2] + "/" + h[2:4] + "/" + h
}
