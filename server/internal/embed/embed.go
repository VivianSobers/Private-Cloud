// Package embed turns document text into vectors and compares them, for semantic
// search: finding a document by what it is ABOUT rather than by a word it
// literally contains.
//
// The actual model runs elsewhere — a GPU inference sidecar the worker calls over
// the tailnet (see client.go), or, on a CPU box, not at all. This package is the
// glue: the interface the rest of the system programs against, the vector math,
// and the chunking, none of which needs a model present. That separation is what
// lets semantic search be a strong GPU feature where the hardware exists and
// cleanly absent where it does not, with the same code.
package embed

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
)

// Embedder produces vectors for text. Implemented by the sidecar HTTP client in
// production and by a deterministic fake in tests, so the pipeline can be
// exercised without a model.
type Embedder interface {
	// Model names the embedding space. Vectors are only comparable within one
	// model, so this is stored with every vector and filtered on at query time.
	Model() string
	// Dim is the vector dimension the model produces.
	Dim() int
	// Embed returns one vector per input text, in order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Pack serializes a vector to little-endian float32 bytes for storage.
func Pack(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// Unpack deserializes packed float32 bytes back to a vector.
func Unpack(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// Literal renders a vector in pgvector's text input format, "[1,2,3]".
//
// The text form rather than the binary one because it is what a plain parameter
// cast to ::vector accepts, so nothing here depends on registering a pgx type
// for an extension that may not be installed. strconv with -1 precision emits
// the shortest string that round-trips the float32 exactly, so the value stored
// in `vec` is bit-identical to the one packed into `vector` and the two ranking
// paths cannot disagree.
func Literal(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*8 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// Cosine is the cosine similarity of two equal-length vectors, in [-1, 1]. A
// zero-magnitude vector has no direction, so its similarity to anything is 0
// rather than a NaN that would poison ranking.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Chunking parameters. Text is embedded in overlapping windows so a query can hit
// the passage that is actually relevant, and the overlap keeps a sentence that
// straddles a boundary from being split out of every window that contains it.
const (
	chunkRunes   = 1500
	chunkOverlap = 200
	// maxChunks caps how many windows one document contributes, so a huge file
	// cannot produce thousands of embed calls and vectors.
	maxChunks = 64
)

// ChunkText splits text into overlapping windows for embedding. Splitting is on
// rune boundaries so a window never ends mid-character. A short text is a single
// chunk.
func ChunkText(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= chunkRunes {
		return []string{text}
	}

	var out []string
	step := chunkRunes - chunkOverlap
	for start := 0; start < len(runes) && len(out) < maxChunks; start += step {
		end := start + chunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return out
}
