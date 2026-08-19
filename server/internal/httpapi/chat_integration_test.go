package httpapi_test

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Asking questions of your own documents.
//
// The properties that matter are not "the answer is good" — that is the model's
// business — but that citations are always present, that retrieval never reaches
// content the caller could not open, and that a missing generator degrades
// instead of removing a feature that works.

// stubGenerator stands in for a GPU sidecar. It echoes which passages it was
// given, so a test can assert what was actually offered as context.
type stubGenerator struct {
	err  error
	seen []embed.Passage
}

func (g *stubGenerator) Model() string { return "stub-generator" }
func (g *stubGenerator) Generate(_ context.Context, question string, passages []embed.Passage) (string, error) {
	if g.err != nil {
		return "", g.err
	}
	g.seen = passages
	refs := make([]string, 0, len(passages))
	for _, p := range passages {
		refs = append(refs, p.Ref)
	}
	return "answer to " + question + " from [" + strings.Join(refs, ",") + "]", nil
}

func TestChatNeedsTheEmbeddingSidecar(t *testing.T) {
	f := newAPIFixture(t)

	// No embedder wired on the default fixture.
	rec := f.json(http.MethodPost, "/api/v1/chat", map[string]any{"question": "anything at all"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("chat without an embedder = %d, want 503", rec.Code)
	}
	if decode(t, rec)["error"].(map[string]any)["code"] != "semantic_unavailable" {
		t.Error("chat should reuse the same stable code the search path uses")
	}
}

func TestChatRejectsATrivialQuestion(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)

	rec := f.json(http.MethodPost, "/api/v1/chat", map[string]any{"question": "a"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("one-character question = %d, want 400", rec.Code)
	}
}

// Retrieval works with no generator at all, and says why there is no answer.
// This is the mode the shipped web client already renders.
func TestChatWithoutAGeneratorStillReturnsCitations(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays and stays shut all weekend")

	rec := f.json(http.MethodPost, "/api/v1/chat",
		map[string]any{"question": "when does the office close on friday"})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)

	if _, ok := body["answer"]; ok {
		t.Error("an answer appeared with no generator configured")
	}
	if body["answer_unavailable"] != "generation_disabled" {
		t.Errorf("answer_unavailable = %v, want generation_disabled", body["answer_unavailable"])
	}
	citations := body["citations"].([]any)
	if len(citations) == 0 {
		t.Fatal("no citations — retrieval is the half that is supposed to work")
	}
	c := citations[0].(map[string]any)
	if c["path"] == "" || c["ref"] == "" {
		t.Errorf("citation is not usable: %v", c)
	}
}

func TestChatAnswersWithCitations(t *testing.T) {
	gen := &stubGenerator{}
	f := newAPIFixtureWithAI(t, gen)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays and stays shut all weekend")

	rec := f.json(http.MethodPost, "/api/v1/chat",
		map[string]any{"question": "when does the office close on friday"})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)

	if body["answer"] == nil || body["answer"] == "" {
		t.Fatal("no answer")
	}
	if body["model"] != "stub-generator" {
		t.Errorf("model = %v, want the generator's identity", body["model"])
	}
	// Citations accompany the answer, always.
	if len(body["citations"].([]any)) == 0 {
		t.Fatal("an answer arrived with no citations")
	}
	// And the generator was handed passage TEXT, not just paths — an answer
	// grounded in filenames alone would be invention.
	if len(gen.seen) == 0 || strings.TrimSpace(gen.seen[0].Text) == "" {
		t.Error("the generator was given no passage text to ground its answer in")
	}
}

// Nothing retrieved means no answer. Answering anyway would be the model
// inventing something, which is the exact failure this design refuses to ship.
func TestChatWithNoMatchesDoesNotInventAnAnswer(t *testing.T) {
	gen := &stubGenerator{}
	f := newAPIFixtureWithAI(t, gen)
	// Nothing indexed at all.

	rec := f.json(http.MethodPost, "/api/v1/chat",
		map[string]any{"question": "what is the vault combination"})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if _, ok := body["answer"]; ok {
		t.Error("an answer was produced with nothing retrieved to ground it")
	}
	if body["answer_unavailable"] != "no_matching_documents" {
		t.Errorf("answer_unavailable = %v, want no_matching_documents", body["answer_unavailable"])
	}
	if len(gen.seen) != 0 {
		t.Error("the generator was called with no passages")
	}
}

// A generator that fails degrades to retrieval rather than failing the request:
// the client already renders citations, and losing the written answer is a
// smaller loss than losing the whole response.
func TestChatDegradesWhenGenerationFails(t *testing.T) {
	gen := &stubGenerator{err: embed.ErrGenerationUnavailable}
	f := newAPIFixtureWithAI(t, gen)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays")

	rec := f.json(http.MethodPost, "/api/v1/chat",
		map[string]any{"question": "when does the office close"})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d, want a degraded 200: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if body["answer_unavailable"] != "generation_unavailable" {
		t.Errorf("answer_unavailable = %v, want generation_unavailable", body["answer_unavailable"])
	}
	if len(body["citations"].([]any)) == 0 {
		t.Error("a failed generation also lost the citations")
	}
}

// The property that stops chat becoming a way to read around a permission.
func TestChatRetrievalIsScopedToTheCaller(t *testing.T) {
	gen := &stubGenerator{}
	f := newAPIFixtureWithAI(t, gen)
	f.indexDoc(t, "secret.txt", "the vault combination is seven three nine")

	// The OTHER account asks. It owns nothing and was granted nothing.
	rec := f.do(http.MethodPost, "/api/v1/chat",
		jsonBody(t, map[string]any{"question": "what is the vault combination"}), f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if len(body["citations"].([]any)) != 0 {
		t.Fatal("another user retrieved passages from a document they cannot open")
	}
	if _, ok := body["answer"]; ok {
		t.Fatal("an answer was generated from another user's documents")
	}
	for _, p := range gen.seen {
		if strings.Contains(p.Text, "seven three nine") {
			t.Fatal("a private passage was handed to the generator for another user")
		}
	}
}

// --- fixture support ---------------------------------------------------------

// bowEmbedder is the same deterministic bag-of-words stand-in the files package
// uses: it hashes each word into a bucket, so texts sharing words get similar
// vectors. Not a real model, but it has the one property these tests need and it
// needs no sidecar.
type bowEmbedder struct{ dim int }

func (b bowEmbedder) Model() string { return "bow-http-" + strconv.Itoa(b.dim) }
func (b bowEmbedder) Dim() int      { return b.dim }
func (b bowEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, b.dim)
		for _, tok := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			h.Write([]byte(tok))
			v[int(h.Sum32())%b.dim]++
		}
		out[i] = v
	}
	return out, nil
}

// newAPIFixtureWithAI enables semantic retrieval, and generation when gen is
// non-nil. The setters take effect on the already-built handler because the
// handlers read these fields per request — the same way the real binary enables
// them once a sidecar has verified.
func newAPIFixtureWithAI(t *testing.T, gen embed.Generator) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	f.srv.SetEmbedder(bowEmbedder{dim: 1024})
	if gen != nil {
		f.srv.SetGenerator(gen)
	}
	return f
}

// indexDoc uploads a document and runs the real extraction and embedding
// handlers over it, so retrieval has something to find.
// A scope this server does not implement is refused by name, not ignored.
//
// The contract sketches scope.node_ids and scope.tags; neither exists. Accepting
// them would mean a caller believing their question was narrowed to three files
// when it was asked of everything they own — and getting a confident answer
// drawn from documents they deliberately excluded.
func TestChatRefusesAScopeItDoesNotImplement(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)

	rec := f.json(http.MethodPost, "/api/v1/chat", map[string]any{
		"question": "what is in the handbook",
		"scope":    map[string]any{"node_ids": []string{"00000000-0000-0000-0000-000000000000"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("chat with an unimplemented scope = %d, want 400", rec.Code)
	}
	msg, _ := decode(t, rec)["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "node_ids") {
		t.Errorf("error does not name the offending field: %q", msg)
	}
}

func (f *apiFixture) indexDoc(t *testing.T, name, body string) {
	t.Helper()

	node, err := f.filesSvc.Upload(f.ctx, f.userID, f.rootID(t), name,
		strings.NewReader(body), "text/plain")
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	extractH := extract.NewHandler(files.NewExtractOpener(f.filesSvc), f.store, extract.New(), nil)
	if err := extractH.Handle(f.ctx, &node.ID, f.userID); err != nil {
		t.Fatalf("extract %s: %v", name, err)
	}
	embedH := embed.NewHandler(f.store, f.store, bowEmbedder{dim: 1024}, nil)
	if err := embedH.Handle(f.ctx, &node.ID, f.userID); err != nil {
		t.Fatalf("embed %s: %v", name, err)
	}
}

// rootID resolves the fixture user's root folder through the store, since these
// helpers work below the HTTP layer.
func (f *apiFixture) rootID(t *testing.T) uuid.UUID {
	t.Helper()
	root, err := f.store.EnsureRoot(f.ctx, f.userID)
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	return root.ID
}

// --- streaming (Phase 8 slice 4) --------------------------------------------

// streamingStub is a generator that emits its answer a word at a time, so the
// ORDER of what reaches the client can be asserted rather than assumed.
type streamingStub struct {
	pieces []string

	err error

	// failAfter emits this many pieces and then fails, which is how a truncated
	// answer is produced on purpose.
	failAfter int
}

func (g *streamingStub) Model() string { return "stub-streamer" }

func (g *streamingStub) Generate(_ context.Context, _ string, _ []embed.Passage) (string, error) {
	if g.err != nil {
		return "", g.err
	}
	return strings.Join(g.pieces, ""), nil
}

func (g *streamingStub) GenerateStream(_ context.Context, _ string, _ []embed.Passage, onDelta func(string) error) error {
	// An error with no failAfter fails before writing anything, which is the
	// case where no prose has reached the client at all.
	if g.err != nil && g.failAfter == 0 {
		return g.err
	}
	for i, p := range g.pieces {
		if g.failAfter > 0 && i >= g.failAfter {
			return embed.ErrGenerationUnavailable
		}
		if err := onDelta(p); err != nil {
			return err
		}
	}
	return g.err
}

// sseEvent is one parsed `event:`/`data:` pair.
type sseEvent struct {
	name string
	data map[string]any
}

func parseSSE(t *testing.T, raw string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(raw), "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data); err != nil {
					t.Fatalf("event %q has undecodable data: %v", ev.name, err)
				}
			}
		}
		if ev.name != "" {
			events = append(events, ev)
		}
	}
	return events
}

// The property the whole feature turns on: every citation is delivered BEFORE
// the first token of prose. An answer that streamed ahead of its citations would
// be, for the duration of the stream, exactly the unverifiable output this design
// refuses to produce.
func TestChatStreamSendsCitationsBeforeAnyProse(t *testing.T) {
	gen := &streamingStub{pieces: []string{"the office ", "closes at six", " [1]"}}
	f := newAPIFixtureWithAI(t, gen)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays and stays shut all weekend")

	rec := f.json(http.MethodPost, "/api/v1/chat", map[string]any{
		"question": "when does the office close on friday",
		"stream":   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("streamed chat = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) == 0 {
		t.Fatal("no events were sent")
	}
	if events[0].name != "citations" {
		t.Fatalf("first event = %q, want citations before anything else", events[0].name)
	}
	if len(events[0].data["citations"].([]any)) == 0 {
		t.Fatal("the citations event carried no citations")
	}

	var answer string
	for _, ev := range events[1:] {
		if ev.name == "delta" {
			answer += ev.data["text"].(string)
		}
	}
	if answer != "the office closes at six [1]" {
		t.Errorf("reassembled answer = %q, want the pieces in order", answer)
	}

	last := events[len(events)-1]
	if last.name != "done" {
		t.Errorf("last event = %q, want done", last.name)
	}
	if last.data["model"] != "stub-streamer" {
		t.Errorf("done carried model %v, want the generator's identity", last.data["model"])
	}
}

// A generator with no streaming support is still a generator: the answer arrives
// as one delta rather than the request failing or falling back to no answer.
func TestChatStreamFallsBackToAWholeAnswer(t *testing.T) {
	gen := &stubGenerator{} // implements Generator, not StreamingGenerator
	f := newAPIFixtureWithAI(t, gen)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays")

	rec := f.json(http.MethodPost, "/api/v1/chat", map[string]any{
		"question": "when does the office close",
		"stream":   true,
	})
	events := parseSSE(t, rec.Body.String())
	if len(events) < 3 || events[0].name != "citations" {
		t.Fatalf("unexpected event sequence: %+v", events)
	}
	var deltas int
	for _, ev := range events {
		if ev.name == "delta" {
			deltas++
		}
	}
	if deltas != 1 {
		t.Errorf("got %d deltas, want the whole answer as one", deltas)
	}
}

// A stream must always terminate with `done`, including when it fails. A stream
// that simply stops is indistinguishable from a dropped connection, and a client
// that cannot tell those apart either hangs or reports a failure that never
// happened.
func TestChatStreamAlwaysTerminates(t *testing.T) {
	// The generator fails before writing a single token: no prose reached the
	// client, so the honest terminator is that no answer is available.
	t.Run("the sidecar fails before writing anything", func(t *testing.T) {
		gen := &streamingStub{pieces: []string{"x"}, err: embed.ErrGenerationUnavailable}
		assertStreamEndsWith(t, gen, true, "generation_unavailable")
	})

	// The generator fails part way through. The client is holding half a
	// paragraph it can see on screen, so reporting the answer as unavailable
	// would be a lie about something visible; it is reported as truncated.
	t.Run("the sidecar fails part way through", func(t *testing.T) {
		gen := &streamingStub{pieces: []string{"half an ", "answer"}, failAfter: 1}
		assertStreamEndsWith(t, gen, true, "generation_truncated")
	})

	// Nothing retrieved. The generator is never called at all, because a model
	// asked to answer from no passages invents one.
	t.Run("nothing was retrieved", func(t *testing.T) {
		gen := &streamingStub{pieces: []string{"unused"}}
		assertStreamEndsWith(t, gen, false, "no_matching_documents")
	})
}

// assertStreamEndsWith drives one streamed question and checks the two
// invariants every failure path shares: citations still come first, and the
// stream still terminates with a `done` naming the reason.
func assertStreamEndsWith(t *testing.T, gen embed.Generator, indexed bool, want string) {
	t.Helper()

	f := newAPIFixtureWithAI(t, gen)
	if indexed {
		f.indexDoc(t, "handbook.txt", "the office closes at six on fridays")
	}

	rec := f.json(http.MethodPost, "/api/v1/chat", map[string]any{
		"question": "when does the office close",
		"stream":   true,
	})
	events := parseSSE(t, rec.Body.String())
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if events[0].name != "citations" {
		t.Errorf("first event = %q, want citations even on the failure paths", events[0].name)
	}
	last := events[len(events)-1]
	if last.name != "done" {
		t.Fatalf("last event = %q, want done", last.name)
	}
	if last.data["answer_unavailable"] != want {
		t.Errorf("answer_unavailable = %v, want %v", last.data["answer_unavailable"], want)
	}
}

// Retrieval-only deployments stream too: a client that set up an event reader
// must not also have to handle a plain JSON body on a server with no generator.
func TestChatStreamWithoutAGenerator(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays")

	rec := f.json(http.MethodPost, "/api/v1/chat", map[string]any{
		"question": "when does the office close",
		"stream":   true,
	})
	events := parseSSE(t, rec.Body.String())
	if len(events) != 2 || events[0].name != "citations" || events[1].name != "done" {
		t.Fatalf("unexpected event sequence: %+v", events)
	}
	if events[1].data["answer_unavailable"] != "generation_disabled" {
		t.Errorf("answer_unavailable = %v, want generation_disabled", events[1].data["answer_unavailable"])
	}
}
