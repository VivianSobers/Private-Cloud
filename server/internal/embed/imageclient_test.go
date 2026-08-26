package embed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// imageSidecar serves the wire shape the reference sidecar serves, so the client
// is tested against the contract rather than against itself.
func imageSidecar(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprintf(w, `{"status":"ok","model":"openai/clip-vit-base-patch32","dim":%d,"device":"cpu"}`, dim)
		case "/embed-image":
			body, _ := io.ReadAll(r.Body)
			// A vector whose first element is the byte count, so the test can
			// prove the bytes actually crossed the wire rather than trusting
			// that a plausible-looking response arrived.
			vec := make([]float32, dim)
			vec[0] = float32(len(body))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"model":"clip-vit-base-patch32","dim":%d,"vector":%s}`, dim, jsonFloats(vec))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func jsonFloats(v []float32) string {
	out := "["
	for i, f := range v {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%g", f)
	}
	return out + "]"
}

func TestImageClientEmbedsRawBytes(t *testing.T) {
	srv := imageSidecar(t, 4)
	defer srv.Close()

	c := NewImageClient(srv.URL, "clip-vit-base-patch32", 4)
	vec, err := c.EmbedImage(context.Background(), "image/jpeg", []byte("\xff\xd8\xff\xe0abc"))
	if err != nil {
		t.Fatalf("EmbedImage: %v", err)
	}
	if len(vec) != 4 {
		t.Fatalf("vector width = %d, want 4", len(vec))
	}
	// The sidecar echoed the body length, so this proves the bytes were posted
	// as-is rather than JSON-wrapped or truncated.
	if vec[0] != 7 {
		t.Errorf("sidecar saw %v bytes, want 7", vec[0])
	}
	if c.Model() != "clip-vit-base-patch32" || c.Dim() != 4 {
		t.Errorf("model/dim wrong: %s/%d", c.Model(), c.Dim())
	}
}

// The width is checked at the client, not passed on. A vector of the wrong width
// stored in the table is invisible to the ranking filter forever, which reads as
// a photo that was never indexed — a failure with no error anywhere.
func TestImageClientRejectsTheWrongWidth(t *testing.T) {
	srv := imageSidecar(t, 8)
	defer srv.Close()

	c := NewImageClient(srv.URL, "clip-vit-base-patch32", 4)
	_, err := c.EmbedImage(context.Background(), "image/png", []byte("x"))
	if !errors.Is(err, ErrImageEmbedUnavailable) {
		t.Fatalf("EmbedImage with a width mismatch = %v, want ErrImageEmbedUnavailable", err)
	}
}

// The sidecar reports what it could not do in a 200 body, the way the detector
// does — so the reason survives into the job's log line instead of being
// flattened into a status code.
func TestImageClientReadsAnErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":"undecodable image: cannot identify image file"}`)
	}))
	defer srv.Close()

	c := NewImageClient(srv.URL, "clip", 4)
	_, err := c.EmbedImage(context.Background(), "image/png", []byte("not an image"))
	if !errors.Is(err, ErrImageEmbedUnavailable) {
		t.Fatalf("error = %v, want ErrImageEmbedUnavailable", err)
	}
	if got := err.Error(); !contains(got, "undecodable") {
		t.Errorf("the sidecar's reason did not survive: %q", got)
	}
}

// An unreachable sidecar is a stable error, never a panic and never a hang past
// the client's own timeout — the phase's rule applied at the lowest level.
func TestImageClientUnreachableDegrades(t *testing.T) {
	c := NewImageClient("http://127.0.0.1:1", "clip", 4)
	if _, err := c.EmbedImage(context.Background(), "image/jpeg", []byte("x")); !errors.Is(err, ErrImageEmbedUnavailable) {
		t.Fatalf("unreachable sidecar = %v, want ErrImageEmbedUnavailable", err)
	}
}

// An unconfigured client is the default state and must not attempt a request.
func TestImageClientUnconfigured(t *testing.T) {
	c := NewImageClient("", "clip", 4)
	if _, err := c.EmbedImage(context.Background(), "image/jpeg", []byte("x")); !errors.Is(err, ErrImageEmbedUnavailable) {
		t.Fatalf("unconfigured client = %v, want ErrImageEmbedUnavailable", err)
	}
}

func TestImageClientVerify(t *testing.T) {
	srv := imageSidecar(t, 512)
	defer srv.Close()

	c := NewImageClient(srv.URL, "clip-vit-base-patch32", 512)
	info, err := c.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// The stored identity drops the vendor prefix; that is the same model.
	if !c.SameImageModel(info.Model) {
		t.Error("SameImageModel must ignore a vendor prefix and case")
	}
	// A sidecar swapped to another model of the same width is exactly the case
	// nothing else in the system can detect.
	if c.SameImageModel("siglip-base-patch16") {
		t.Error("SameImageModel must not treat a different model as a match")
	}

	// A width mismatch is distinguished from a transport failure, because the
	// worker disables the handler for one and registers anyway for the other.
	narrow := NewImageClient(srv.URL, "clip-vit-base-patch32", 4)
	if _, err := narrow.Verify(context.Background()); !errors.Is(err, ErrDimMismatch) {
		t.Fatalf("Verify with a width mismatch = %v, want ErrDimMismatch", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
