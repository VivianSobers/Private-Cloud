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

// Client is an Embedder backed by the inference sidecar — a small service that
// loads the model (on a GPU where one exists) and answers /embed over HTTP. The
// API and the worker hold this, never a model: an RPC to a sidecar is not a
// resident model, which is what keeps the always-on box's memory budget intact.
type Client struct {
	base  string
	model string
	dim   int
	http  *http.Client
}

// NewClient builds a sidecar client. model and dim are what the sidecar is known
// to serve; they are stored with every vector so search never compares across
// spaces. A short timeout: embedding a query is on the synchronous search path,
// and a slow sidecar should fail fast to a lexical fallback, not hang the search.
func NewClient(base, model string, dim int) *Client {
	return &Client{
		base:  base,
		model: model,
		dim:   dim,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Model() string { return c.model }
func (c *Client) Dim() int      { return c.dim }

type embedRequest struct {
	Texts []string `json:"texts"`
}

type embedResponse struct {
	Model   string      `json:"model"`
	Dim     int         `json:"dim"`
	Vectors [][]float32 `json:"vectors"`
}

// Embed sends texts to the sidecar and returns their vectors. It verifies the
// shape of what comes back — one vector per text, each of the expected dimension
// — so a sidecar misconfiguration surfaces here rather than as corrupt rows or a
// panic deep in the ranking math.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Texts: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("embed sidecar: status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed sidecar: decode: %w", err)
	}
	if len(out.Vectors) != len(texts) {
		return nil, fmt.Errorf("embed sidecar: got %d vectors for %d texts", len(out.Vectors), len(texts))
	}
	for i, v := range out.Vectors {
		if len(v) != c.dim {
			return nil, fmt.Errorf("embed sidecar: vector %d has dim %d, expected %d", i, len(v), c.dim)
		}
	}
	return out.Vectors, nil
}

// SidecarInfo is what the sidecar reports about itself at /healthz.
type SidecarInfo struct {
	Status string `json:"status"`
	Model  string `json:"model"`
	Dim    int    `json:"dim"`
	Device string `json:"device"`
}

// Verify probes the sidecar and checks that what it serves is what this client
// is configured to store.
//
// The dimension check is the hard one: a width mismatch means every vector
// written here would be rejected on arrival, and every query would be filtered
// out at search time — semantic search that returns nothing, with no error
// anywhere. Refusing to start is better than serving an empty feature.
//
// The model is returned rather than enforced, because the identity vectors are
// stored under is deliberately an operator-chosen string, not the sidecar's
// HuggingFace id. The caller compares them and warns: swapping the sidecar to a
// DIFFERENT model of the SAME width is the case that silently mixes two
// unrelated vector spaces under one name, and nothing else in the system can
// detect it.
func (c *Client) Verify(ctx context.Context) (SidecarInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return SidecarInfo{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return SidecarInfo{}, fmt.Errorf("embed sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SidecarInfo{}, fmt.Errorf("embed sidecar: healthz returned %d", resp.StatusCode)
	}

	var info SidecarInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&info); err != nil {
		return SidecarInfo{}, fmt.Errorf("embed sidecar: decode healthz: %w", err)
	}
	if info.Dim != c.dim {
		return info, fmt.Errorf(
			"%w: sidecar serves %q at dim %d, but PC_EMBED_DIM is %d — every vector would be rejected and every query would match nothing",
			ErrDimMismatch, info.Model, info.Dim, c.dim)
	}
	return info, nil
}

// ErrDimMismatch means the sidecar's vector width is not the configured one.
// Distinguished from a transport failure so a caller can refuse to enable a
// feature that would silently return nothing, while still tolerating a sidecar
// that is merely slow to start.
var ErrDimMismatch = errors.New("embed sidecar dimension mismatch")

// SameModel reports whether the sidecar's reported model is the one this client
// stores vectors under, ignoring a vendor prefix and case — "BAAI/bge-small-en-v1.5"
// is the same model as the identity "bge-small-en-v1.5". A false here means the
// sidecar was swapped without changing PC_EMBED_MODEL, so new vectors would land
// in the same logical space as vectors from a different model.
func (c *Client) SameModel(sidecarModel string) bool {
	norm := func(s string) string {
		if i := strings.LastIndex(s, "/"); i >= 0 {
			s = s[i+1:]
		}
		return strings.ToLower(strings.TrimSpace(s))
	}
	return norm(sidecarModel) == norm(c.model)
}
