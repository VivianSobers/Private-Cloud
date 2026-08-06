// Package chunk is the client half of the delta protocol's chunking. It uses the
// SAME content-defined boundaries, BLAKE3 addressing and zstd handling the server
// does, because those parameters are the protocol now, not an implementation
// detail.
//
// A client that chunked differently would still produce correct files — the
// whole-file BLAKE3 is a hash of the bytes, independent of where the boundaries
// fall — but it would dedup against nothing the server already holds. Matching
// the server's FastCDC parameters is exactly what turns "sync a 4 GB file that
// changed by one block" into moving one block.
package chunk

import (
	"context"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/tigerwill90/fastcdc"
	"github.com/zeebo/blake3"
)

// Chunking parameters. These MUST match server/internal/cas: 2 KiB minimum, 16
// KiB average, 64 KiB maximum. Below the threshold a file is a whole blob, not a
// manifest — a manifest row plus a chunk row to describe 900 bytes is pure
// overhead and dedups nothing.
const (
	MinSize            = 2 << 10  // 2 KiB
	AvgSize            = 16 << 10 // 16 KiB
	MaxSize            = 64 << 10 // 64 KiB
	WholeFileThreshold = MinSize
)

// Compression identifiers, as the server reports them in X-Chunk-Compression.
const (
	CompressionNone = "none"
	CompressionZstd = "zstd"
)

// Chunk is one content-defined piece of a file as the client sees it.
//
// Plain is the plaintext the client uploads — the server compresses and
// addresses it, and re-verifies the address, so the client never sends stored
// bytes. Plain is owned by the splitter and is valid ONLY for the duration of
// the callback; anything that must outlive the callback has to be copied.
type Chunk struct {
	Hash   [32]byte
	Offset int64
	Size   int
	Plain  []byte
}

// Fn receives each chunk in file order.
type Fn func(*Chunk) error

// Split divides r into content-defined chunks and returns BLAKE3 of the whole
// reassembled file.
//
// The whole-file hash is the file's content address — the same value the server
// stores as a manifest's content_hash — so a commit the client builds from these
// chunks binds to exactly the bytes it read.
func Split(ctx context.Context, r io.Reader, fn Fn) ([32]byte, error) {
	splitter, err := fastcdc.NewChunker(ctx,
		fastcdc.WithChunksSize(MinSize, AvgSize, MaxSize))
	if err != nil {
		return [32]byte{}, fmt.Errorf("create chunker: %w", err)
	}

	whole := blake3.New()
	emit := func(offset, length uint, plain []byte) error {
		// The library reuses its internal buffer, so plain is valid only inside
		// this callback. The whole-file hash is fed here; per-chunk work the
		// caller needs beyond the call is its responsibility to copy.
		whole.Write(plain)
		return fn(&Chunk{
			Hash:   blake3.Sum256(plain),
			Offset: int64(offset),
			Size:   len(plain),
			Plain:  plain,
		})
	}

	if err := splitter.Split(r, emit); err != nil {
		return [32]byte{}, fmt.Errorf("split: %w", err)
	}
	// Finalize emits the trailing partial chunk. Skipping it silently truncates
	// every file whose length is not a chunk boundary — nearly all of them.
	if err := splitter.Finalize(emit); err != nil {
		return [32]byte{}, fmt.Errorf("finalize split: %w", err)
	}

	var out [32]byte
	copy(out[:], whole.Sum(nil))
	return out, nil
}

// --- decompression ----------------------------------------------------------

// sharedDecoder is reused across chunk downloads: zstd decoders are safe for
// concurrent use, and allocating one per small chunk would dominate the read.
var sharedDecoder *zstd.Decoder

func init() {
	// A decoder with no dictionary and default settings never fails to build; the
	// error is checked only to satisfy the signature.
	d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		panic("chunk: cannot build zstd decoder: " + err.Error())
	}
	sharedDecoder = d
}

// Decompress returns the plaintext of a stored chunk the server served, per the
// compression it reported. The caller verifies the result against the chunk's
// address — the same self-check the server runs on every read.
func Decompress(stored []byte, compression string, plainSize int) ([]byte, error) {
	switch compression {
	case CompressionNone, "":
		return stored, nil
	case CompressionZstd:
		out, err := sharedDecoder.DecodeAll(stored, make([]byte, 0, plainSize))
		if err != nil {
			return nil, fmt.Errorf("decompress chunk: %w", err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown compression %q", compression)
	}
}
