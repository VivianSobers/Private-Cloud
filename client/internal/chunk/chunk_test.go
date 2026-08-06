package chunk

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/zeebo/blake3"
)

// Chunking must reassemble to exactly the input, in order, and the whole-file
// hash must equal a plain BLAKE3 of the bytes — that value is the file's content
// address, so anything else would bind a commit to the wrong content.
func TestSplitReassemblesAndAddresses(t *testing.T) {
	data := bytes.Repeat([]byte("private cloud sync payload "), 8000) // ~216 KiB, several chunks

	var (
		reassembled []byte
		count       int
		lastEnd     int64
	)
	whole, err := Split(context.Background(), bytes.NewReader(data), func(c *Chunk) error {
		if c.Offset != lastEnd {
			t.Errorf("chunk at offset %d, expected %d — a gap or overlap", c.Offset, lastEnd)
		}
		if blake3.Sum256(c.Plain) != c.Hash {
			t.Error("chunk hash does not address its plaintext")
		}
		reassembled = append(reassembled, c.Plain...)
		lastEnd = c.Offset + int64(c.Size)
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if count < 2 {
		t.Fatalf("expected multiple chunks, got %d", count)
	}
	if !bytes.Equal(reassembled, data) {
		t.Error("reassembled chunks differ from the input")
	}
	if want := blake3.Sum256(data); hex.EncodeToString(whole[:]) != hex.EncodeToString(want[:]) {
		t.Error("whole-file hash is not BLAKE3 of the content")
	}
}

// Splitting is deterministic: the same bytes produce the same boundaries every
// time. If it were not, two devices would never share a chunk.
func TestSplitDeterministic(t *testing.T) {
	data := bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44, 0x55}, 40000)
	run := func() []string {
		var hs []string
		if _, err := Split(context.Background(), bytes.NewReader(data), func(c *Chunk) error {
			hs = append(hs, hex.EncodeToString(c.Hash[:]))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return hs
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("chunk counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

func TestDecompressRoundTrip(t *testing.T) {
	// A "none" chunk is returned as-is; the empty string is treated as none, since
	// a whole-file blob download carries no compression header.
	plain := []byte("uncompressed content")
	for _, comp := range []string{CompressionNone, ""} {
		out, err := Decompress(plain, comp, len(plain))
		if err != nil {
			t.Fatalf("decompress %q: %v", comp, err)
		}
		if !bytes.Equal(out, plain) {
			t.Errorf("decompress %q changed the bytes", comp)
		}
	}
	if _, err := Decompress(plain, "brotli", len(plain)); err == nil {
		t.Error("unknown compression should be rejected")
	}
}
