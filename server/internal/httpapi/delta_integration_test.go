package httpapi_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/zeebo/blake3"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
)

// chunkLocally splits data the way a sync client would and returns the ordered
// hex hashes, the plaintext per hash, and the whole-file hash.
func chunkLocally(t *testing.T, data []byte) (order []string, plain map[string][]byte, content string) {
	t.Helper()
	plain = map[string][]byte{}
	whole, err := cas.Split(context.Background(), bytes.NewReader(data), func(c *cas.Chunk) error {
		p, err := cas.Decompress(c.Stored, c.Compression, c.Size)
		if err != nil {
			return err
		}
		h := hex.EncodeToString(c.Hash[:])
		order = append(order, h)
		plain[h] = bytes.Clone(p)
		return nil
	})
	if err != nil {
		t.Fatalf("chunk locally: %v", err)
	}
	return order, plain, hex.EncodeToString(whole[:])
}

// The whole delta protocol over HTTP: negotiate missing chunks, upload only
// those, commit a manifest into a file, then read it back — both whole and chunk
// by chunk — and confirm every byte survives.
func TestDeltaProtocolOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	// Unique content so its chunks are genuinely new in the shared database.
	data := append([]byte(uuid.NewString()), bytes.Repeat([]byte("delta sync payload "), 12000)...)
	order, plain, content := chunkLocally(t, data)
	if len(order) < 2 {
		t.Fatalf("expected several chunks, got %d", len(order))
	}

	// 1. Everything is missing to start.
	rec := f.json(http.MethodPost, "/api/v1/chunks/have", map[string]any{"hashes": order})
	missing := toStrings(decode(t, rec)["missing"].([]any))
	if len(missing) != len(order) {
		t.Fatalf("have reported %d/%d missing before upload", len(missing), len(order))
	}

	// 2. Upload exactly the missing chunks.
	for _, h := range missing {
		rec = f.do(http.MethodPut, "/api/v1/chunks/"+h, bytes.NewReader(plain[h]), f.cookie)
		if rec.Code != http.StatusCreated {
			t.Fatalf("put chunk %s = %d: %s", h, rec.Code, rec.Body)
		}
	}

	// 3. Now nothing is missing.
	rec = f.json(http.MethodPost, "/api/v1/chunks/have", map[string]any{"hashes": order})
	if n := len(toStrings(decode(t, rec)["missing"].([]any))); n != 0 {
		t.Fatalf("%d chunks still missing after upload", n)
	}

	// 4. Commit the manifest into a file.
	rec = f.json(http.MethodPost, "/api/v1/manifests?parent_id="+root+"&name=big.bin",
		map[string]any{"content_hash": content, "chunks": order})
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit manifest = %d: %s", rec.Code, rec.Body)
	}
	id := nodeID(t, rec)

	// 5. The whole file reads back byte-for-byte.
	rec = f.do(http.MethodGet, "/api/v1/nodes/"+id+"/content", nil, f.cookie)
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Error("downloaded file differs from the committed content")
	}

	// 6. The manifest lists the same chunks, and each fetches and verifies.
	rec = f.do(http.MethodGet, "/api/v1/nodes/"+id+"/manifest", nil, f.cookie)
	man := decode(t, rec)
	if man["kind"] != "chunked" {
		t.Fatalf("manifest kind = %v, want chunked", man["kind"])
	}
	for _, ce := range man["chunks"].([]any) {
		h := ce.(map[string]any)["hash"].(string)
		rec = f.do(http.MethodGet, "/api/v1/chunks/"+h, nil, f.cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("get chunk %s = %d", h, rec.Code)
		}
		got, err := cas.Decompress(rec.Body.Bytes(), rec.Header().Get("X-Chunk-Compression"), len(plain[h]))
		if err != nil {
			t.Fatalf("decompress chunk %s: %v", h, err)
		}
		if sum := blake3.Sum256(got); hex.EncodeToString(sum[:]) != h {
			t.Errorf("chunk %s failed verification after download", h)
		}
	}
}

func TestPutChunkRejectsMismatchOverHTTP(t *testing.T) {
	f := newAPIFixture(t)
	data := []byte(uuid.NewString() + " honest bytes")
	sum := blake3.Sum256(data)
	wrong := sum
	wrong[0] ^= 0xff

	rec := f.do(http.MethodPut, "/api/v1/chunks/"+hex.EncodeToString(wrong[:]),
		bytes.NewReader(data), f.cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("put with a wrong hash = %d, want 400", rec.Code)
	}
}

func TestGetChunkRequiresReference(t *testing.T) {
	// Uploading a chunk does not, by itself, grant the right to read it back: a
	// user may fetch a chunk only once they hold a file made of it.
	f := newAPIFixture(t)
	root := nodeID(t, f.do(http.MethodGet, "/api/v1/nodes/root", nil, f.cookie))

	data := []byte(uuid.NewString() + " single-chunk payload")
	sum := blake3.Sum256(data)
	h := hex.EncodeToString(sum[:])

	if rec := f.do(http.MethodPut, "/api/v1/chunks/"+h, bytes.NewReader(data), f.cookie); rec.Code != http.StatusCreated {
		t.Fatalf("put chunk = %d", rec.Code)
	}
	// Uploaded but not referenced by any file: reading it is refused as not-found.
	if rec := f.do(http.MethodGet, "/api/v1/chunks/"+h, nil, f.cookie); rec.Code != http.StatusNotFound {
		t.Errorf("unreferenced chunk was readable: %d", rec.Code)
	}

	// Commit a file made of it; now the reference exists and the read succeeds.
	rec := f.json(http.MethodPost, "/api/v1/manifests?parent_id="+root+"&name=one.bin",
		map[string]any{"content_hash": h, "chunks": []string{h}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("commit = %d: %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodGet, "/api/v1/chunks/"+h, nil, f.cookie); rec.Code != http.StatusOK {
		t.Errorf("referenced chunk was not readable: %d", rec.Code)
	}
}

func toStrings(a []any) []string {
	out := make([]string, len(a))
	for i, v := range a {
		out[i] = v.(string)
	}
	return out
}
