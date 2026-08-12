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

// The generation half of RAG.
//
// Deliberately the same shape as the embedding sidecar: a small service on a box
// with a GPU, spoken to over HTTP, holding the model so this process never does.
// The API and the worker must stay free of resident models — that is the whole
// reason the two-tier split exists, and a 7 GiB always-on box cannot host a
// generator without giving up being always on.
//
// Content never leaves your infrastructure. This client talks to a sidecar you
// run; there is no hosted-provider path here, and adding one would break the
// promise the whole project is built on.

// ErrGenerationUnavailable means no generator is configured or it could not be
// reached. Always surfaced as 503 with a stable code, never a 500 and never a
// hang — the file API must stay fully functional with all of this switched off.
var ErrGenerationUnavailable = errors.New("generation is not available")

// Generator composes an answer from a question and the passages retrieved for
// it. An interface so tests can drive the chat endpoint without a GPU.
type Generator interface {
	Generate(ctx context.Context, question string, passages []Passage) (string, error)
	Model() string
}

// Passage is one retrieved chunk offered to the generator as context.
type Passage struct {
	// Ref is the citation marker the model is told to use, e.g. "1".
	Ref  string
	Path string
	Text string
}

// GenClient is a Generator backed by an HTTP sidecar.
type GenClient struct {
	base  string
	model string
	http  *http.Client
}

// NewGenClient builds a generation client.
//
// A far longer timeout than the embedding client's 30s: embedding a query is a
// single forward pass on the synchronous search path, while generating an answer
// is hundreds of sequential token steps. Failing that fast would make the feature
// unusable on exactly the modest hardware it is aimed at.
func NewGenClient(base, model string) *GenClient {
	return &GenClient{
		base:  base,
		model: model,
		http:  &http.Client{Timeout: 5 * time.Minute},
	}
}

func (c *GenClient) Model() string { return c.model }

type generateRequest struct {
	Question string    `json:"question"`
	Passages []Passage `json:"passages"`
	Model    string    `json:"model,omitempty"`
}

type generateResponse struct {
	Answer string `json:"answer"`
	Error  string `json:"error,omitempty"`
}

// Generate asks the sidecar for an answer grounded in the passages.
//
// The prompt itself lives in the sidecar, not here. Prompt wording is the part
// most likely to need tuning against a specific model, and rebuilding and
// redeploying the Go API to change a sentence of English would be the wrong
// place to put that iteration loop.
func (c *GenClient) Generate(ctx context.Context, question string, passages []Passage) (string, error) {
	if c.base == "" {
		return "", ErrGenerationUnavailable
	}

	body, err := json.Marshal(generateRequest{
		Question: question, Passages: passages, Model: c.model,
	})
	if err != nil {
		return "", err
	}

	url := strings.TrimRight(c.base, "/") + "/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGenerationUnavailable, err)
	}
	defer resp.Body.Close()

	// Bounded: a sidecar that streams forever must not be able to exhaust the
	// API's memory, which is the resource the whole tier split exists to protect.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGenerationUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: sidecar returned %d", ErrGenerationUnavailable, resp.StatusCode)
	}

	var out generateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("%w: malformed response", ErrGenerationUnavailable)
	}
	if out.Error != "" {
		return "", fmt.Errorf("%w: %s", ErrGenerationUnavailable, out.Error)
	}
	if strings.TrimSpace(out.Answer) == "" {
		// An empty answer alongside no error is a broken sidecar, and returning it
		// would render as a confident blank rather than as a failure.
		return "", fmt.Errorf("%w: empty answer", ErrGenerationUnavailable)
	}
	return out.Answer, nil
}
