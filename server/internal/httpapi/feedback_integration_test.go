package httpapi_test

import (
	"net/http"
	"net/url"
	"testing"
)

// Feedback on machine output (Phase 8, open item 9).
//
// The properties that matter are not "somebody clicked a button". They are that
// a judgement is durable and readable back by the person who made it, that it
// never leaks across owners, that judging something you cannot read is answered
// exactly as if it did not exist, and — the part that makes this more than a
// survey — that a result marked wrong stops coming back.

func TestFeedbackRecordsAndReadsBack(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)

	rec := f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
		"kind":    "answer",
		"context": "when does the office close",
		"verdict": "not_helpful",
		"note":    "it answered about the wrong building",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit feedback = %d: %s", rec.Code, rec.Body)
	}
	got := decode(t, rec)["feedback"].(map[string]any)
	if got["verdict"] != "not_helpful" || got["kind"] != "answer" {
		t.Errorf("feedback came back changed: %v", got)
	}
	if got["note"] != "it answered about the wrong building" {
		t.Errorf("the note is the only place a person can say WHAT was wrong, and it was lost: %v", got)
	}

	rec = f.do(http.MethodGet, "/api/v1/feedback", nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list feedback = %d: %s", rec.Code, rec.Body)
	}
	list := decode(t, rec)["feedback"].([]any)
	if len(list) != 1 {
		t.Fatalf("read back %d judgements, want the 1 that was submitted", len(list))
	}
}

// Changing your mind replaces the judgement rather than adding a second one.
// Two standing verdicts on one target by one person have no defensible
// resolution, and it is what makes the suppression reversible without a delete
// endpoint.
func TestFeedbackOnTheSameTargetReplacesItself(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays")
	node := f.searchHit(t, "office closes")

	for _, verdict := range []string{"wrong", "helpful"} {
		rec := f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
			"kind": "search", "node_id": node, "verdict": verdict,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("submit %s = %d: %s", verdict, rec.Code, rec.Body)
		}
	}

	rec := f.do(http.MethodGet, "/api/v1/feedback?kind=search", nil, f.cookie)
	list := decode(t, rec)["feedback"].([]any)
	if len(list) != 1 {
		t.Fatalf("two verdicts on one target left %d rows, want 1", len(list))
	}
	if v := list[0].(map[string]any)["verdict"]; v != "helpful" {
		t.Errorf("standing verdict = %v, want the most recent one", v)
	}
}

// A judgement describes a person's opinion, not the bytes, so it must not cross
// owners — the same rule migration 00023 states for faces, and the reason this
// table is per-owner rather than content-addressed.
func TestFeedbackIsPerOwner(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)

	rec := f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
		"kind": "answer", "context": "who signed the lease", "verdict": "wrong",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit feedback = %d: %s", rec.Code, rec.Body)
	}

	rec = f.do(http.MethodGet, "/api/v1/feedback", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("list feedback as another user = %d: %s", rec.Code, rec.Body)
	}
	if list := decode(t, rec)["feedback"].([]any); len(list) != 0 {
		t.Fatalf("another account read %d of this user's judgements", len(list))
	}
}

// You may only judge a result you could already read, and "no access" and "no
// such node" are the same answer. Anything else turns the feedback endpoint into
// an existence oracle for other people's files — the probe /similar's read check
// on its source exists to close.
func TestFeedbackOnSomebodyElsesNodeIsIndistinguishableFromNoSuchNode(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)
	f.indexDoc(t, "secret.txt", "the vault combination is seven three nine")
	node := f.searchHit(t, "vault combination")

	denied := f.do(http.MethodPost, "/api/v1/feedback",
		jsonBody(t, map[string]any{"kind": "search", "node_id": node, "verdict": "wrong"}), f.admin)
	missing := f.do(http.MethodPost, "/api/v1/feedback",
		jsonBody(t, map[string]any{
			"kind":    "search",
			"node_id": "00000000-0000-0000-0000-000000000000",
			"verdict": "wrong",
		}), f.admin)

	if denied.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("denied = %d, missing = %d; both must be 404", denied.Code, missing.Code)
	}
	deniedCode := decode(t, denied)["error"].(map[string]any)["code"]
	missingCode := decode(t, missing)["error"].(map[string]any)["code"]
	if deniedCode != missingCode {
		t.Errorf("a file you may not read answers %v and a file that does not exist answers %v — "+
			"the difference is the oracle", deniedCode, missingCode)
	}
}

func TestFeedbackRefusesAVerdictItDoesNotUnderstand(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)

	rec := f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
		"kind": "answer", "context": "anything", "verdict": "meh",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown verdict = %d, want 400", rec.Code)
	}
	if decode(t, rec)["error"].(map[string]any)["code"] != "invalid_feedback" {
		t.Error("the refusal should carry a stable code a client can branch on")
	}
}

// A kind that names a file needs one. Without this the row records a judgement
// about nothing and the suppression predicate silently never matches it.
func TestFeedbackNeedsTheThingItIsAbout(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)

	rec := f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
		"kind": "similar", "verdict": "wrong",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("targetless feedback = %d, want 400: %s", rec.Code, rec.Body)
	}
}

// The effect that makes this more than a survey: a semantic hit marked wrong by
// its owner does not come back, and marking it helpful again brings it back.
// Reversibility is why there is no delete endpoint.
func TestAWrongSearchHitIsSuppressedForItsOwner(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)
	f.indexDoc(t, "boiler.txt", "the plumber quoted two thousand to replace the boiler")

	node := f.searchHit(t, "plumber quoted boiler")
	if node == "" {
		t.Fatal("nothing was retrieved to give feedback on")
	}

	rec := f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
		"kind": "search", "node_id": node, "verdict": "wrong",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit feedback = %d: %s", rec.Code, rec.Body)
	}

	if again := f.searchHit(t, "plumber quoted boiler"); again == node {
		t.Fatal("a hit its owner marked wrong came back on the next search")
	}

	// Reversible: the standing verdict is replaced, and so is its effect.
	rec = f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
		"kind": "search", "node_id": node, "verdict": "helpful",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("changing the verdict = %d: %s", rec.Code, rec.Body)
	}
	if again := f.searchHit(t, "plumber quoted boiler"); again != node {
		t.Error("marking a result helpful again did not lift the suppression")
	}
}

// Suppression is scoped to the kind of result that was judged. "This is not like
// my file" is a claim about similarity, not a claim that the document is a poor
// source for an unrelated question — letting one dismissed neighbour delete a
// document from somebody's answers would be the feature quietly destroying data
// it was given no permission to touch.
func TestFeedbackSuppressionDoesNotCrossKinds(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays and stays shut all weekend")

	node := f.searchHit(t, "office closes fridays")
	if node == "" {
		t.Fatal("nothing was retrieved to give feedback on")
	}
	rec := f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
		"kind": "similar", "node_id": node, "verdict": "wrong",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit feedback = %d: %s", rec.Code, rec.Body)
	}

	rec = f.json(http.MethodPost, "/api/v1/chat",
		map[string]any{"question": "when does the office close on friday"})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d: %s", rec.Code, rec.Body)
	}
	if len(decode(t, rec)["citations"].([]any)) == 0 {
		t.Error("a 'similar' verdict removed the document from chat retrieval as well")
	}
}

// And the kind that DOES apply to chat: a citation marked wrong stops being
// retrieved for the person who marked it.
func TestAWrongCitationIsNotRetrievedAgain(t *testing.T) {
	f := newAPIFixtureWithAI(t, nil)
	f.indexDoc(t, "handbook.txt", "the office closes at six on fridays and stays shut all weekend")

	rec := f.json(http.MethodPost, "/api/v1/chat",
		map[string]any{"question": "when does the office close on friday"})
	citations := decode(t, rec)["citations"].([]any)
	if len(citations) == 0 {
		t.Fatal("nothing was cited to give feedback on")
	}
	node := citations[0].(map[string]any)["node_id"].(string)

	if rec := f.json(http.MethodPost, "/api/v1/feedback", map[string]any{
		"kind": "citation", "node_id": node, "verdict": "wrong",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("submit feedback = %d: %s", rec.Code, rec.Body)
	}

	rec = f.json(http.MethodPost, "/api/v1/chat",
		map[string]any{"question": "when does the office close on friday"})
	for _, c := range decode(t, rec)["citations"].([]any) {
		if c.(map[string]any)["node_id"] == node {
			t.Fatal("a citation its owner marked wrong was retrieved again")
		}
	}
}

// searchHit runs a semantic search and returns the top hit's node id, or "" when
// nothing matched. The suppression tests are about what comes back, so they need
// the same read path a person uses rather than a store call underneath it.
func (f *apiFixture) searchHit(t *testing.T, query string) string {
	t.Helper()
	rec := f.do(http.MethodGet, "/api/v1/search?semantic=true&q="+url.QueryEscape(query), nil, f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("semantic search = %d: %s", rec.Code, rec.Body)
	}
	results, _ := decode(t, rec)["results"].([]any)
	if len(results) == 0 {
		return ""
	}
	id, _ := results[0].(map[string]any)["id"].(string)
	return id
}
