package cas

import (
	"bytes"
	"context"
	"encoding/hex"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/zeebo/blake3"
)

// deterministic pseudo-random data, so a failure is reproducible.
func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rng := rand.New(rand.NewSource(seed))
	rng.Read(b)
	return b
}

func chunkAll(t *testing.T, data []byte) []*Chunk {
	t.Helper()
	out, _ := chunkAllWithHash(t, data)
	return out
}

func chunkAllWithHash(t *testing.T, data []byte) ([]*Chunk, [32]byte) {
	t.Helper()
	var out []*Chunk
	hash, err := Split(context.Background(), bytes.NewReader(data), func(ch *Chunk) error {
		out = append(out, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	return out, hash
}

func TestChunksReassembleExactly(t *testing.T) {
	data := randBytes(1<<20, 1) // 1 MiB

	var got bytes.Buffer
	var offset int64
	for _, ch := range chunkAll(t, data) {
		if ch.Offset != offset {
			t.Fatalf("chunk offset = %d, want %d — offsets must be contiguous or Range breaks", ch.Offset, offset)
		}
		plain, err := Decompress(ch.Stored, ch.Compression, ch.Size)
		if err != nil {
			t.Fatalf("Decompress: %v", err)
		}
		got.Write(plain)
		offset += int64(ch.Size)
	}

	if !bytes.Equal(got.Bytes(), data) {
		t.Fatal("reassembled content does not match the input")
	}
}

func TestChunkBoundsAreRespected(t *testing.T) {
	chunks := chunkAll(t, randBytes(2<<20, 2))
	if len(chunks) < 2 {
		t.Fatalf("2 MiB produced %d chunks", len(chunks))
	}
	for i, ch := range chunks {
		if ch.Size > MaxChunkSize {
			t.Errorf("chunk %d is %d bytes, over the %d maximum", i, ch.Size, MaxChunkSize)
		}
		// The last chunk is whatever remains and may legitimately be short.
		if i < len(chunks)-1 && ch.Size < MinChunkSize {
			t.Errorf("chunk %d is %d bytes, under the %d minimum", i, ch.Size, MinChunkSize)
		}
	}
}

func TestInsertionShiftsOneBoundaryNotAll(t *testing.T) {
	// The entire justification for content-defined chunking. Fixed-size blocks
	// would dedup nothing here, because every boundary after the insert moves.
	original := randBytes(1<<20, 3)

	modified := make([]byte, 0, len(original)+16)
	modified = append(modified, original[:4096]...)
	modified = append(modified, []byte("SIXTEEN BYTES!!!")...)
	modified = append(modified, original[4096:]...)

	before := map[string]bool{}
	for _, ch := range chunkAll(t, original) {
		before[hex.EncodeToString(ch.Hash[:])] = true
	}

	after := chunkAll(t, modified)
	var shared int
	for _, ch := range after {
		if before[hex.EncodeToString(ch.Hash[:])] {
			shared++
		}
	}

	ratio := float64(shared) / float64(len(after))
	if ratio < 0.8 {
		t.Errorf("only %.0f%% of chunks survived a 16-byte insertion (%d of %d); "+
			"content-defined chunking is not working", ratio*100, shared, len(after))
	}
}

func TestIdenticalContentProducesIdenticalHashes(t *testing.T) {
	// Dedup depends on this being true across processes and across users.
	data := randBytes(256<<10, 4)

	first := chunkAll(t, data)
	second := chunkAll(t, data)

	if len(first) != len(second) {
		t.Fatalf("chunk counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Hash != second[i].Hash {
			t.Fatalf("chunk %d hashed differently on a second pass", i)
		}
	}
}

func TestCompressibleContentIsCompressed(t *testing.T) {
	chunks := chunkAll(t, bytes.Repeat([]byte("the same line over and over\n"), 20000))

	var compressed int
	for _, ch := range chunks {
		if ch.Compression == CompressionZstd {
			compressed++
			if len(ch.Stored) >= ch.Size {
				t.Errorf("a 'compressed' chunk grew: %d >= %d", len(ch.Stored), ch.Size)
			}
		}
	}
	if compressed == 0 {
		t.Error("highly repetitive content was not compressed at all")
	}
}

func TestIncompressibleContentIsStoredRaw(t *testing.T) {
	// Random data stands in for JPEG/MP4/.zst: paying decompression on every
	// read to store slightly MORE bytes would be strictly worse.
	for _, ch := range chunkAll(t, randBytes(512<<10, 5)) {
		if ch.Compression != CompressionNone {
			t.Errorf("random data was stored as %s", ch.Compression)
		}
		if len(ch.Stored) != ch.Size {
			t.Errorf("uncompressed chunk stored %d bytes for %d of content", len(ch.Stored), ch.Size)
		}
	}
}

func TestContentHashCoversWholeFile(t *testing.T) {
	data := randBytes(300<<10, 6)
	_, got := chunkAllWithHash(t, data)

	// Compared against the library directly, NOT against another run of the
	// chunker — a self-comparison would pass even if both were wrong, and the
	// download ETag depends on this being a real BLAKE3 of the file.
	if want := blake3.Sum256(data); got != want {
		t.Errorf("whole-file hash = %x, want BLAKE3 of the input %x", got, want)
	}
}

func TestEmptyInputProducesNoChunks(t *testing.T) {
	if got := chunkAll(t, nil); len(got) != 0 {
		t.Errorf("empty input produced %d chunks", len(got))
	}
}

func TestConcurrentChunkersAreSafe(t *testing.T) {
	// This is why the chunking library was swapped. jotfs/fastcdc-go mutates a
	// package-level gearing table inside NewChunker, which races against the
	// table reads every other in-flight chunker performs in its inner loop.
	// Run under -race, this test is what proves the replacement is clean.
	data := randBytes(128<<10, 7)
	want := chunkAll(t, data)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var n int
			_, err := Split(context.Background(), bytes.NewReader(data), func(ch *Chunk) error {
				if n < len(want) && ch.Hash != want[n].Hash {
					return errChunkMismatch
				}
				n++
				return nil
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent chunking: %v", err)
	}
}

var errChunkMismatch = errorString("concurrent chunkers produced different hashes for identical input")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestStorageKeyLayout(t *testing.T) {
	var h [32]byte
	copy(h[:], []byte{0xab, 0xcd, 0xef})

	key := StorageKey(h)
	if !strings.HasPrefix(key, "ab/cd/abcdef") {
		t.Errorf("StorageKey = %q, want the ab/cd/<full hex> layout", key)
	}
	if parts := strings.Split(key, "/"); len(parts) != 3 || len(parts[2]) != 64 {
		t.Errorf("StorageKey = %q, want three parts ending in a 64-char hash", key)
	}
}

func TestDecompressRejectsUnknownCompression(t *testing.T) {
	if _, err := Decompress([]byte("x"), "lzma", 1); err == nil {
		t.Error("an unknown compression name was accepted")
	}
}
