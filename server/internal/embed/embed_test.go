package embed

import (
	"math"
	"strings"
	"testing"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	v := []float32{0, 1, -1, 3.14159, 1e-9, -2.5e8}
	got := Unpack(Pack(v))
	if len(got) != len(v) {
		t.Fatalf("length changed: %d != %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("index %d: %v != %v", i, got[i], v[i])
		}
	}
}

func TestCosine(t *testing.T) {
	a := []float32{1, 0, 0}
	cases := []struct {
		name string
		b    []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, 1},
		{"orthogonal", []float32{0, 1, 0}, 0},
		{"opposite", []float32{-1, 0, 0}, -1},
		{"scaled", []float32{5, 0, 0}, 1},
		{"zero", []float32{0, 0, 0}, 0},
	}
	for _, c := range cases {
		if got := Cosine(a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: cosine = %v, want %v", c.name, got, c.want)
		}
	}
	// Mismatched lengths are 0, never a panic or NaN.
	if got := Cosine([]float32{1, 2}, []float32{1}); got != 0 {
		t.Errorf("mismatched lengths: %v, want 0", got)
	}
}

func TestChunkText(t *testing.T) {
	if ChunkText("   ") != nil {
		t.Error("blank text should chunk to nil")
	}
	if got := ChunkText("short doc"); len(got) != 1 || got[0] != "short doc" {
		t.Errorf("short text should be one chunk: %v", got)
	}

	long := strings.Repeat("a", 4000)
	chunks := ChunkText(long)
	if len(chunks) < 2 {
		t.Fatalf("long text should produce multiple chunks, got %d", len(chunks))
	}
	// Windows overlap, so consecutive chunks share their boundary region and the
	// union still covers the whole text.
	for _, c := range chunks {
		if len([]rune(c)) > chunkRunes {
			t.Errorf("chunk exceeds window: %d runes", len([]rune(c)))
		}
	}
	// The first window starts at the beginning and the last reaches the end.
	if !strings.HasPrefix(long, chunks[0]) {
		t.Error("first chunk is not the head of the text")
	}
	if !strings.HasSuffix(long, chunks[len(chunks)-1]) {
		t.Error("last chunk is not the tail of the text")
	}
}
