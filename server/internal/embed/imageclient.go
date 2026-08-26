package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Image embeddings (Phase 8, slice 5) — the fourth sidecar.
//
// A separate service from the text embedder rather than another endpoint on it,
// for the reason the detection sidecar is separate: the models are different
// sizes with different resource profiles, and a deployment may reasonably want
// semantic document search without wanting a vision encoder resident on a box.
// Nothing here is required for any other feature to work.

// ErrImageEmbedUnavailable means no image-embedding sidecar is configured or it
// could not be reached. Like every other Phase 8 dependency this degrades: the
// file API, the gallery, thumbnails and text-space similarity are unaffected by
// its absence, and /similar keeps ranking in the document space.
var ErrImageEmbedUnavailable = errors.New("image embedding is not available")

// ImageEmbedder turns image bytes into one vector. An interface so the handler
// can be tested without a GPU, exactly as Embedder and media.Detector are.
type ImageEmbedder interface {
	// Model names the image-embedding space. Vectors are only comparable within
	// one model, so this is stored with every vector and filtered on at ranking
	// time.
	Model() string
	// Dim is the vector width the model produces.
	Dim() int
	// EmbedImage returns one vector for one image.
	EmbedImage(ctx context.Context, mime string, data []byte) ([]float32, error)
}

// ImageClient is an ImageEmbedder backed by the HTTP sidecar, the same shape as
// Client and media.DetectClient: the model lives on a box with a GPU and this
// process holds none of it.
type ImageClient struct {
	base  string
	model string
	dim   int
	http  *http.Client
}

// NewImageClient builds an image-embedding sidecar client. model and dim are
// what the sidecar is known to serve; they are stored with every vector so
// ranking never compares across spaces.
func NewImageClient(base, model string, dim int) *ImageClient {
	return &ImageClient{
		base:  base,
		model: model,
		dim:   dim,
		// Two minutes, matching the detector rather than the text embedder's
		// thirty seconds. This is a background job that decodes an image and
		// runs a vision model over it, never a synchronous request path — and
		// /similar reads the stored vector rather than calling here, so nobody
		// is waiting on this timeout.
		http: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (c *ImageClient) Model() string { return c.model }
func (c *ImageClient) Dim() int      { return c.dim }

// imageEmbedResponse is the sidecar's reply. `error` is carried in a 200 body
// the way the detector does it, so the reason survives into the job's log line
// instead of being flattened into a status code.
type imageEmbedResponse struct {
	Model  string    `json:"model"`
	Dim    int       `json:"dim"`
	Vector []float32 `json:"vector"`
	Error  string    `json:"error,omitempty"`
}

// EmbedImage posts the raw image bytes to the sidecar and reads back one vector.
//
// Raw bytes with the image's own Content-Type, not JSON or multipart: a
// base64-encoded 40 MiB JPEG is 53 MiB of string on both sides of a link that
// may be a tailnet hop between two houses, and the detector already established
// this wire shape.
//
// Every failure is wrapped in ErrImageEmbedUnavailable so the caller has one
// thing to test for. A width mismatch is checked here rather than passed on:
// storing a vector of the wrong width would put a row in the table that the
// ranking filter excludes forever, which reads as a photo that was never
// indexed and is the failure with no error anywhere.
func (c *ImageClient) EmbedImage(ctx context.Context, mime string, data []byte) ([]float32, error) {
	if c.base == "" {
		return nil, ErrImageEmbedUnavailable
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty image", ErrImageEmbedUnavailable)
	}

	url := strings.TrimRight(c.base, "/") + "/embed-image"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mime)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrImageEmbedUnavailable, err)
	}
	defer resp.Body.Close()

	// Bounded: one vector is a few tens of kilobytes of JSON, so anything past a
	// megabyte is a service that is not this one.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrImageEmbedUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: sidecar returned %d: %s",
			ErrImageEmbedUnavailable, resp.StatusCode, bytes.TrimSpace(raw))
	}

	var out imageEmbedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: malformed response", ErrImageEmbedUnavailable)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrImageEmbedUnavailable, out.Error)
	}
	if len(out.Vector) != c.dim {
		return nil, fmt.Errorf("%w: vector has dim %d, expected %d",
			ErrImageEmbedUnavailable, len(out.Vector), c.dim)
	}
	return out.Vector, nil
}

// Verify probes /healthz and checks the sidecar serves the width this client is
// configured to store, for Client.Verify's reason: a width mismatch means every
// vector is rejected on arrival and every ranking filter excludes the rest, so
// the feature returns nothing forever with no error anywhere. Refusing to
// register the handler is better than serving an empty feature.
//
// The model is reported rather than enforced, again as Client.Verify does — the
// identity is an operator-chosen string, not the sidecar's HuggingFace id, and
// the caller compares them with SameImageModel and warns.
func (c *ImageClient) Verify(ctx context.Context) (SidecarInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.base, "/")+"/healthz", nil)
	if err != nil {
		return SidecarInfo{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return SidecarInfo{}, fmt.Errorf("%w: %v", ErrImageEmbedUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SidecarInfo{}, fmt.Errorf("%w: healthz returned %d", ErrImageEmbedUnavailable, resp.StatusCode)
	}

	var info SidecarInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&info); err != nil {
		return SidecarInfo{}, fmt.Errorf("%w: decode healthz: %v", ErrImageEmbedUnavailable, err)
	}
	if info.Dim != c.dim {
		return info, fmt.Errorf(
			"%w: sidecar serves %q at dim %d, but PC_IMAGE_EMBED_DIM is %d — every vector would be rejected and no photo would ever rank",
			ErrDimMismatch, info.Model, info.Dim, c.dim)
	}
	return info, nil
}

// SameImageModel reports whether the sidecar's reported model is the one this
// client stores vectors under, ignoring a vendor prefix and case — so
// "openai/clip-vit-base-patch32" is the same model as the identity
// "clip-vit-base-patch32". A false means the sidecar was swapped without
// changing PC_IMAGE_EMBED_MODEL, which silently mixes two unrelated vector
// spaces under one name and is the one thing nothing else in the system can
// detect.
func (c *ImageClient) SameImageModel(sidecarModel string) bool {
	norm := func(s string) string {
		if i := strings.LastIndex(s, "/"); i >= 0 {
			s = s[i+1:]
		}
		return strings.ToLower(strings.TrimSpace(s))
	}
	return norm(sidecarModel) == norm(c.model)
}
