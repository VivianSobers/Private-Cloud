package files

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Feedback: what a person told this server about something it produced.
//
// The word usually means "labels for the next training run", and that is exactly
// what this is not — Phase 4 put model training out of scope and nothing here
// changes that. What it is instead is the half of the idea that is in scope and
// still worth having: a durable record of a judgement, readable back by the
// person who made it, which the retrieval layer consults before it answers them
// again. The loop closes in the query, not in a model, and that is the whole
// difference between a feature and a survey.
//
// Per-owner, for the reason migration 00023 gives about faces: a vector describes
// bytes and may be shared between owners, but a judgement describes a person's
// opinion and must not leak across them.

// The kinds of machine output a person can judge. Each names a result shape this
// server produces today; there is deliberately no "other", because a kind
// nothing emits is a row nothing will ever read back.
const (
	FeedbackAnswer   = "answer"   // a written /chat answer, as a whole
	FeedbackCitation = "citation" // one document a /chat answer cited
	FeedbackSimilar  = "similar"  // one neighbour from /nodes/{id}/similar
	FeedbackSearch   = "search"   // one semantic search hit
	FeedbackFace     = "face"     // a face or cluster assignment
)

// The verdicts. Three, not a star rating: "not helpful" and "wrong" are
// different claims about a result and only one of them has an effect. A document
// can be entirely correct and useless for the question asked, and suppressing
// those would punish retrieval for how somebody phrased a question.
const (
	VerdictHelpful    = "helpful"
	VerdictNotHelpful = "not_helpful"
	VerdictWrong      = "wrong"
)

// maxFeedbackNote matches the CHECK in migration 00030. Duplicated here so the
// caller gets a 400 naming the field rather than a constraint violation surfacing
// as a 500 — the database is the guarantee, this is the error message.
const maxFeedbackNote = 500

var (
	// ErrInvalidFeedback means the kind, verdict or note does not describe
	// anything this server could have produced.
	ErrInvalidFeedback = errors.New("invalid feedback")
	// ErrFeedbackTargetRequired means the kind needs a target it was not given.
	ErrFeedbackTargetRequired = errors.New("feedback needs a target")
)

// Feedback is one person's standing judgement on one machine-produced result.
//
// Standing, not historical: re-submitting replaces it (migration 00030's unique
// constraint), because two live verdicts on the same target by the same person
// have no defensible resolution. It is also what makes the effect reversible —
// marking something helpful again lifts the suppression, with no delete endpoint
// to get wrong.
type Feedback struct {
	ID        uuid.UUID
	OwnerID   uuid.UUID
	Kind      string
	NodeID    *uuid.UUID
	PersonID  *uuid.UUID
	Context   string
	Verdict   string
	Note      string
	CreatedAt time.Time
	UpdatedAt time.Time

	// Path and Name of the target node, when it is a node and still exists.
	// Carried so a read-back is legible without a second round trip per row: a
	// list of uuids is not something a person can review.
	Path string
	Name string
}

// validFeedbackKind and validVerdict keep the Go side honest about the closed
// sets the schema enforces.
func validFeedbackKind(kind string) bool {
	switch kind {
	case FeedbackAnswer, FeedbackCitation, FeedbackSimilar, FeedbackSearch, FeedbackFace:
		return true
	}
	return false
}

func validVerdict(v string) bool {
	switch v {
	case VerdictHelpful, VerdictNotHelpful, VerdictWrong:
		return true
	}
	return false
}

// RecordFeedback stores or replaces one judgement.
//
// The ACCESS CHECK IS NOT HERE. It belongs to the handler, which owns the rule
// that "no access" and "no such node" are the same answer and which has already
// resolved the target before it gets this far. Doing it in both places would mean
// two implementations of one access rule, which is how they come to disagree.
func (s *Store) RecordFeedback(ctx context.Context, in Feedback) (*Feedback, error) {
	in.Kind = strings.TrimSpace(in.Kind)
	in.Verdict = strings.TrimSpace(in.Verdict)
	in.Note = strings.TrimSpace(in.Note)
	in.Context = strings.TrimSpace(in.Context)

	if !validFeedbackKind(in.Kind) || !validVerdict(in.Verdict) {
		return nil, ErrInvalidFeedback
	}
	if len([]rune(in.Note)) > maxFeedbackNote {
		return nil, ErrInvalidFeedback
	}
	switch in.Kind {
	case FeedbackAnswer:
		// An answer is not a row anywhere — it is a sentence that existed once —
		// so the question is what identifies it, and without one the record names
		// nothing at all.
		if in.NodeID != nil || in.PersonID != nil || in.Context == "" {
			return nil, ErrFeedbackTargetRequired
		}
	case FeedbackFace:
		if in.PersonID == nil {
			return nil, ErrFeedbackTargetRequired
		}
	default:
		if in.NodeID == nil {
			return nil, ErrFeedbackTargetRequired
		}
	}

	// ON CONFLICT DO UPDATE rather than a read-then-write: two tabs open on the
	// same answer would otherwise race to insert and one of them would meet the
	// unique constraint as a 500.
	row := s.pool.QueryRow(ctx, `
		INSERT INTO feedback (owner_id, kind, node_id, person_id, context, verdict, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owner_id, kind, node_id, person_id, context) DO UPDATE
		SET verdict = EXCLUDED.verdict, note = EXCLUDED.note, updated_at = now()
		RETURNING id, owner_id, kind, node_id, person_id, context, verdict, note,
		          created_at, updated_at`,
		in.OwnerID, in.Kind, in.NodeID, in.PersonID, in.Context, in.Verdict, in.Note)

	var out Feedback
	if err := row.Scan(&out.ID, &out.OwnerID, &out.Kind, &out.NodeID, &out.PersonID,
		&out.Context, &out.Verdict, &out.Note, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListFeedback returns one person's own judgements, newest first.
//
// One person's own, with no parameter that could widen it. There is no
// administrative view of everybody's feedback, and this signature is where that
// is decided: an operator reading what individual users called wrong is a
// different feature with a different consent story, and adding it later should
// require writing a new query rather than passing a different argument.
//
// kind, when non-empty, narrows to one kind.
func (s *Store) ListFeedback(ctx context.Context, ownerID uuid.UUID, kind string, limit, offset int) ([]*Feedback, error) {
	if kind != "" && !validFeedbackKind(kind) {
		return nil, ErrInvalidFeedback
	}
	if limit <= 0 || limit > maxFeedbackPage {
		limit = maxFeedbackPage
	}
	if offset < 0 {
		offset = 0
	}

	// LEFT JOIN, so a judgement about a file that has since been trashed still
	// reads back. The row is the person's, not the file's, and dropping it
	// because the target moved to the trash would quietly rewrite what they said.
	rows, err := s.pool.Query(ctx, `
		SELECT f.id, f.owner_id, f.kind, f.node_id, f.person_id, f.context,
		       f.verdict, f.note, f.created_at, f.updated_at,
		       coalesce(n.path, ''), coalesce(n.name, '')
		FROM feedback f
		LEFT JOIN nodes n ON n.id = f.node_id
		WHERE f.owner_id = $1 AND ($2 = '' OR f.kind = $2)
		ORDER BY f.created_at DESC, f.id
		LIMIT $3 OFFSET $4`, ownerID, kind, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Feedback, 0, limit)
	for rows.Next() {
		var f Feedback
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.Kind, &f.NodeID, &f.PersonID,
			&f.Context, &f.Verdict, &f.Note, &f.CreatedAt, &f.UpdatedAt,
			&f.Path, &f.Name); err != nil {
			return nil, err
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

// maxFeedbackPage bounds a read-back. A person's own feedback is small by
// nature, and the cap exists so an unbounded page cannot be asked for rather
// than because anyone is expected to reach it.
const maxFeedbackPage = 100

// NotMarkedWrong is the SQL predicate that makes feedback more than a survey.
//
// It is spliced into the vector queries exactly the way Visibility is, and for
// the same reason: there must be ONE definition of it. The decision table already
// says two nearly identical scans drift and that for an ACL filter drift means a
// leak; a suppression rule written out three times would drift in the milder but
// still bad direction, where a result somebody told us was wrong keeps coming
// back through whichever path forgot.
//
// The kind is a parameter rather than "any wrong verdict" so a judgement stays
// scoped to the question it answered. "This is not like my file" is a claim about
// similarity, not a claim that the document is a poor source for an unrelated
// question, and letting it suppress chat retrieval would let one dismissed
// thumbnail quietly delete a document from a person's answers.
//
// ownerParam and kindParam are PLACEHOLDER NAMES ($1, $6) in the query this is
// spliced into, never values — a value interpolated into SQL here would be an
// injection in the one query that also carries the ACL filter. Passing a kind
// that is the empty string is therefore safe and suppresses nothing, which is
// what a caller with no feedback story wants.
func NotMarkedWrong(ownerParam, kindParam string) string {
	return `NOT EXISTS (
			SELECT 1 FROM feedback fb
			WHERE fb.owner_id = ` + ownerParam + `
			  AND fb.node_id = n.id
			  AND fb.kind = ` + kindParam + `
			  AND fb.verdict = 'wrong')`
}
