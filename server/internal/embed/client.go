package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Healthy reports whether the sidecar answers its health probe, so the API can
// decide at query time whether semantic search is available.
func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
