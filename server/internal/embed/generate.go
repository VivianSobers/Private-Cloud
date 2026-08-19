package embed

import (
	"bufio"
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

// StreamingGenerator emits an answer in pieces as the model produces them.
//
// Optional, and deliberately a separate interface: a Generator that cannot
// stream is still a complete generator, and the endpoint falls back to asking
// for the whole answer and delivering it as one piece. That keeps a sidecar an
// operator already runs working after this shipped, which is the property the
// whole sidecar contract is supposed to have.
//
// onDelta is called in order, on the request's goroutine. Returning an error
// from it stops the generation — that is how a client disconnecting stops the
// work rather than letting a model keep spending a GPU on a browser tab that
// closed.
type StreamingGenerator interface {
	GenerateStream(ctx context.Context, question string, passages []Passage, onDelta func(string) error) error
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
	// Asks the sidecar for newline-delimited JSON instead of one object. A
	// sidecar that does not know the field ignores it and answers whole, which
	// the reader below handles — so an older sidecar keeps working and simply
	// arrives all at once.
	Stream bool `json:"stream,omitempty"`
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

// streamChunk is one line of the sidecar's newline-delimited stream: a piece of
// the answer, an error, or the end. `answer` is accepted too, so a sidecar that
// ignores `stream` and replies with the ordinary whole-answer object is read
// correctly rather than treated as a protocol violation.
type streamChunk struct {
	Delta  string `json:"delta,omitempty"`
	Answer string `json:"answer,omitempty"`
	Done   bool   `json:"done,omitempty"`
	Error  string `json:"error,omitempty"`
}

// GenerateStream asks the sidecar for the answer in pieces.
//
// NDJSON rather than SSE between server and sidecar: this hop is machine to
// machine on your own tailnet, and SSE's event names and retry semantics buy
// nothing here while costing a parser. The browser-facing hop upstream IS SSE,
// because that is what a browser can consume without a library.
//
// The delta is passed on as it arrives and never buffered into a full answer
// first, which would reintroduce exactly the latency streaming exists to remove.
func (c *GenClient) GenerateStream(ctx context.Context, question string, passages []Passage, onDelta func(string) error) error {
	if c.base == "" {
		return ErrGenerationUnavailable
	}

	body, err := json.Marshal(generateRequest{
		Question: question, Passages: passages, Model: c.model, Stream: true,
	})
	if err != nil {
		return err
	}

	url := strings.TrimRight(c.base, "/") + "/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson, application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGenerationUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: sidecar returned %d", ErrGenerationUnavailable, resp.StatusCode)
	}

	// Bounded the same way the non-streaming path is, but per line and in total:
	// a sidecar looping forever must not be able to hold the API open or grow its
	// memory, and a stream has no Content-Length to check first.
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxStreamBytes))
	scanner.Buffer(make([]byte, 0, 8<<10), maxStreamLine)

	var got bool
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			return fmt.Errorf("%w: malformed stream", ErrGenerationUnavailable)
		}
		if chunk.Error != "" {
			return fmt.Errorf("%w: %s", ErrGenerationUnavailable, chunk.Error)
		}
		// `answer` is the whole-answer shape, which a sidecar that ignored the
		// stream flag will have sent. Treat it as one delta and finish.
		for _, piece := range []string{chunk.Delta, chunk.Answer} {
			if piece == "" {
				continue
			}
			got = true
			if err := onDelta(piece); err != nil {
				return err
			}
		}
		if chunk.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrGenerationUnavailable, err)
	}
	if !got {
		// Same rule as the whole-answer path: nothing at all, with no error, is a
		// broken sidecar rather than an empty but valid answer.
		return fmt.Errorf("%w: empty answer", ErrGenerationUnavailable)
	}
	return nil
}

const (
	// One line is one token or a short run of them; 256 KiB is far past any
	// legitimate piece and still small enough to be harmless.
	maxStreamLine = 256 << 10
	// The same 1 MiB ceiling the whole-answer path applies, so streaming cannot
	// become a way around it.
	maxStreamBytes = 1 << 20
)
