-- +goose Up
-- Phase 8: a person's judgement on something the machine produced.
--
-- What this is NOT is the thing "feedback" usually means. Phase 4 put model
-- training out of scope, so nothing here retrains anything, and a table whose
-- only purpose was to accumulate rows for a training run that will never happen
-- would be a survey pretending to be a feature. What it IS: a durable, reviewable
-- record of what a person told us was wrong, which the retrieval layer reads back
-- on the next query. The loop closes in the query planner, not in a model.
--
-- PER-OWNER and NOT content-addressed, for exactly the reason migration 00023
-- gives for faces. A document's vector describes its bytes and two owners of the
-- same file may safely share one row; a judgement describes a PERSON'S OPINION of
-- a result, and one user marking a neighbour wrong must never change what a
-- stranger who happens to own the same bytes is shown. Sharing these rows would
-- also leak the shape of one library into another's results, which is the same
-- class of leak the ACL filter on the vector scan exists to prevent.

CREATE TABLE feedback (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The actor and the owner are the same person by construction, so this is one
    -- column rather than two. You may only judge a result you could already see,
    -- the record belongs to the person who made it, and a judgement somebody else
    -- made is not yours to be served back — there is no third party whose identity
    -- a separate actor column could hold. CASCADE because a judgement has no
    -- meaning once the person who formed it is gone; unlike audit_log, nobody is
    -- ever accountable to this table.
    owner_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Which machine-produced thing was judged. Closed set, because a kind this
    -- server cannot produce is a row nothing will ever read: every value here
    -- names a result shape that exists today.
    --
    --   answer   a written /chat answer, taken as a whole
    --   citation one document a /chat answer cited
    --   similar  one neighbour returned by /nodes/{id}/similar
    --   search   one hit from semantic search
    --   face     a face or cluster assignment
    kind text NOT NULL CHECK (kind IN ('answer', 'citation', 'similar', 'search', 'face')),

    -- The target. A node for the three result kinds that name a file, a cluster
    -- for 'face', and neither for 'answer' — an answer is not a row anywhere, it
    -- is a sentence that existed once, so what identifies it is the question that
    -- produced it and that lives in `context`.
    --
    -- CASCADE on both: feedback about a deleted file is feedback about nothing,
    -- and keeping it would suppress whatever later takes the id.
    node_id   uuid REFERENCES nodes (id)  ON DELETE CASCADE,
    person_id uuid REFERENCES people (id) ON DELETE CASCADE,

    -- What the machine was answering when it produced the target: the question
    -- asked, the search query, or the id of the node a /similar call started
    -- from. Free text and deliberately not a foreign key — it records what the
    -- person was doing, which is context for a human reading the record back, not
    -- something any query joins on.
    context text NOT NULL DEFAULT '',

    -- Three verdicts, not a star rating. 'not_helpful' and 'wrong' are different
    -- claims and collapsing them would lose the only one that has an effect: a
    -- result can be perfectly correct and still useless for the question asked,
    -- and demoting those would punish the retrieval for the asker's phrasing.
    -- Only 'wrong' suppresses.
    verdict text NOT NULL CHECK (verdict IN ('helpful', 'not_helpful', 'wrong')),

    -- Optional, short, and the whole reason this is reviewable rather than a
    -- counter: "wrong" tells an operator that something failed, and the note is
    -- the only place the person can say what. Bounded because a text box with no
    -- limit in a table nothing summarises is a place for an essay nobody reads.
    note text NOT NULL DEFAULT '' CHECK (length(note) <= 500),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- The shape each kind must have, enforced here rather than in Go. A 'face'
    -- row with no cluster or a 'similar' row with no node is a record of a
    -- judgement about nothing, and the suppression predicate would silently never
    -- match it.
    CHECK (
        (kind = 'answer' AND node_id IS NULL AND person_id IS NULL)
        OR (kind = 'face' AND person_id IS NOT NULL)
        OR (kind IN ('citation', 'similar', 'search') AND node_id IS NOT NULL AND person_id IS NULL)
    ),

    -- Changing your mind is an UPDATE, not a second row. Two standing verdicts on
    -- one target for one person have no defensible resolution — and it is what
    -- makes the effect reversible without a delete endpoint: marking a result
    -- helpful again un-suppresses it, which is the correction path a permanent,
    -- append-only judgement would not have.
    --
    -- NULLS NOT DISTINCT because node_id and person_id are null for the kinds
    -- that do not use them, and under the default rule two 'answer' verdicts on
    -- the same question would both be "distinct" and both stand.
    UNIQUE NULLS NOT DISTINCT (owner_id, kind, node_id, person_id, context)
);

-- The suppression lookup, which runs inside the vector scan for every retrieval
-- this owner performs. Partial, because the rows it must find are the minority —
-- most feedback is not a complaint — and an index over the rest would be carried
-- on the hot path for nothing.
CREATE INDEX feedback_suppressed ON feedback (owner_id, kind, node_id)
    WHERE verdict = 'wrong' AND node_id IS NOT NULL;

-- "What have I told this server?" — the read-back, newest first.
CREATE INDEX feedback_owner ON feedback (owner_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS feedback;
