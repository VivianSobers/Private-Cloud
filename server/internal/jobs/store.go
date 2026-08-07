// Package jobs is a durable, Postgres-backed work queue.
//
// It exists so that intelligence — OCR, embeddings — runs OUT of band: enqueued
// as a row here by the API when a file lands, and drained by a separate worker
// process that is the only thing on the box allowed to load a model. On hardware
// this tight (7 GiB, one spinner) that separation is not a nicety; it is what
// lets the clever features be turned off without touching the file API at all.
//
// The claim is a single `UPDATE ... FOR UPDATE SKIP LOCKED` so two workers never
// take the same job, and a crashed worker's job is returned to the queue by the
// reaper rather than lost — the same transactional discipline the change journal
// uses, applied to compute instead of ordering.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job states.
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

// ErrNoJob is returned by Claim when the queue has nothing runnable.
var ErrNoJob = errors.New("no runnable job")

// Job is one unit of deferred work.
type Job struct {
	ID          uuid.UUID
	Kind        string
	NodeID      *uuid.UUID
	OwnerID     uuid.UUID
	State       string
	Attempts    int
	MaxAttempts int
	RunAfter    time.Time
	LastError   string
	CreatedAt   time.Time
}

// Store is the queue.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// EnqueueOptions tunes a single enqueue.
type EnqueueOptions struct {
	// MaxAttempts overrides the default retry budget when > 0.
	MaxAttempts int
	// Delay defers the job's first eligibility by this much.
	Delay time.Duration
	// OwnerQueueCap, when > 0, refuses the enqueue if the owner already has this
	// many queued jobs — a per-owner bound so one account cannot flood the single
	// worker and starve everyone else's indexing.
	OwnerQueueCap int
}

// ErrOwnerQueueFull is returned when OwnerQueueCap would be exceeded.
var ErrOwnerQueueFull = errors.New("owner has too many queued jobs")

// Enqueue adds a job, unless an identical (kind, node) job is already pending —
// the unique partial index makes that a no-op rather than a duplicate, so a burst
// of edits to one file does not queue a burst of duplicate extraction. The
// returned bool reports whether a new job was actually created.
func (s *Store) Enqueue(ctx context.Context, kind string, nodeID *uuid.UUID, ownerID uuid.UUID, opts EnqueueOptions) (uuid.UUID, bool, error) {
	if opts.OwnerQueueCap > 0 {
		var queued int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM jobs WHERE owner_id = $1 AND state = 'queued'`, ownerID).
			Scan(&queued); err != nil {
			return uuid.Nil, false, err
		}
		if queued >= opts.OwnerQueueCap {
			return uuid.Nil, false, ErrOwnerQueueFull
		}
	}

	maxAttempts := 5
	if opts.MaxAttempts > 0 {
		maxAttempts = opts.MaxAttempts
	}
	runAfter := time.Now().Add(opts.Delay)

	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO jobs (kind, node_id, owner_id, max_attempts, run_after)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (kind, node_id) WHERE state IN ('queued','running')
		DO NOTHING
		RETURNING id`,
		kind, nodeID, ownerID, maxAttempts, runAfter).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// A pending job for this (kind, node) already exists: nothing to do.
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("enqueue: %w", err)
	}
	return id, true, nil
}

// Claim atomically takes the oldest runnable job for the given kinds and marks it
// running. It returns ErrNoJob when the queue has nothing eligible.
//
// The claim is the load-bearing query: the subselect finds one queued job whose
// run_after has passed, locks that row FOR UPDATE SKIP LOCKED so a concurrent
// worker skips it rather than blocking, and the outer UPDATE flips it to running
// in the same statement. There is no window in which two workers hold the same
// job.
func (s *Store) Claim(ctx context.Context, kinds []string) (Job, error) {
	var j Job
	err := s.pool.QueryRow(ctx, `
		UPDATE jobs SET
			state = 'running',
			attempts = attempts + 1,
			claimed_at = now(),
			updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE state = 'queued' AND run_after <= now() AND kind = ANY($1)
			ORDER BY run_after
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, kind, node_id, owner_id, state, attempts, max_attempts, run_after, coalesce(last_error,''), created_at`,
		kinds).Scan(&j.ID, &j.Kind, &j.NodeID, &j.OwnerID, &j.State, &j.Attempts, &j.MaxAttempts, &j.RunAfter, &j.LastError, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNoJob
	}
	if err != nil {
		return Job{}, fmt.Errorf("claim: %w", err)
	}
	return j, nil
}

// Complete marks a claimed job done.
func (s *Store) Complete(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET state = 'done', last_error = NULL, updated_at = now() WHERE id = $1`, id)
	return err
}

// Fail records an error and either reschedules the job with backoff or, once its
// attempts are spent, dead-letters it. A dead-lettered job stays as a record of
// what went wrong rather than vanishing or looping.
//
// Backoff is exponential in the attempt count, capped, so a persistently failing
// job backs off toward the cap instead of retrying at wire speed and pinning the
// one spare core.
func (s *Store) Fail(ctx context.Context, id uuid.UUID, cause error) error {
	// The backoff is an SQL expression over the row's own attempts count —
	// 2^attempts seconds, capped at an hour — so it is inlined, not bound: a bind
	// parameter is a value, and this is an expression the database must evaluate.
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET
			state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
			run_after = now() + LEAST(power(2, attempts) * interval '1 second', interval '1 hour'),
			last_error = $2,
			updated_at = now()
		WHERE id = $1`,
		id, truncErr(cause))
	return err
}

// ReapStale returns jobs stuck in 'running' longer than the lease to the queue —
// the recovery path for a worker that crashed mid-job. A returned job keeps its
// incremented attempts, so a job that reliably crashes its worker still
// dead-letters rather than looping forever.
func (s *Store) ReapStale(ctx context.Context, lease time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET
			state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
			last_error = 'worker lease expired',
			updated_at = now()
		WHERE state = 'running' AND claimed_at < now() - $1::interval`,
		lease.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteFinished prunes done and failed jobs older than retention, so the table
// does not grow without bound. Failed jobs are kept as long as done ones — the
// record of a failure is worth exactly as much as the record of a success — and
// both age out together.
func (s *Store) DeleteFinished(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM jobs
		WHERE state IN ('done','failed') AND updated_at < now() - $1::interval`,
		retention.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteStaleQueued drops jobs that have sat 'queued' longer than ttl without
// ever being claimed.
//
// This is the backstop for a job kind no deployed worker handles. Claim filters
// by the kinds a worker has registered, so a job whose handler exists on no box
// is never claimed, never fails, and never reaches the done/failed states
// DeleteFinished prunes — it accumulates one row per upload, forever. The
// split-tier design makes that easy to hit: PC_ENABLE_SEMANTIC on the always-on
// box enqueues embed jobs for a GPU worker that may never be deployed.
//
// Deleting queued work is lossy, so the caller logs it loudly: a nonzero count
// means the queue is being fed work nothing drains, which is a deployment fault
// to fix rather than a number to watch climb.
func (s *Store) DeleteStaleQueued(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM jobs
		WHERE state = 'queued' AND attempts = 0 AND created_at < now() - $1::interval`,
		ttl.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// EnqueueForAllFiles inserts a job of `kind` for every live file, skipping any
// file that already has one pending — the bulk re-index primitive behind
// `cloudctl jobs reindex`, for rebuilding extracted text or embeddings after a
// model or extractor change. Idempotent by the same unique-pending index that
// dedups ordinary enqueues. Returns how many jobs were created.
func (s *Store) EnqueueForAllFiles(ctx context.Context, kind string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO jobs (kind, node_id, owner_id)
		SELECT $1, n.id, n.owner_id
		FROM nodes n
		WHERE n.kind = 'file' AND n.trashed_at IS NULL
		ON CONFLICT (kind, node_id) WHERE state IN ('queued','running') DO NOTHING`, kind)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListFailed returns dead-lettered jobs, newest first — the operator's view of
// what the JobsDeadLettering alert is warning about.
func (s *Store) ListFailed(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, node_id, owner_id, state, attempts, max_attempts, run_after, coalesce(last_error,''), created_at
		FROM jobs WHERE state = 'failed'
		ORDER BY updated_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.NodeID, &j.OwnerID, &j.State,
			&j.Attempts, &j.MaxAttempts, &j.RunAfter, &j.LastError, &j.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RetryFailed returns dead-lettered jobs to the queue with a fresh attempt
// budget — the escape hatch after fixing whatever made them fail (a sidecar that
// was down, say). Returns how many were requeued.
//
// kind, when non-empty, restricts the retry to one job kind. That matters
// because the reasons jobs fail are per-kind: a sidecar outage dead-letters
// embeds and leaves extractions alone, and requeuing everything then re-runs
// OCR that failed for its own, unfixed reason — burning the one spare core on
// work that will fail again. An unfiltered retry is still available by passing
// an empty kind, which is what "retry everything" should have to say explicitly.
func (s *Store) RetryFailed(ctx context.Context, kind string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs SET
			state = 'queued', attempts = 0, run_after = now(), last_error = NULL, updated_at = now()
		WHERE state = 'failed' AND ($1 = '' OR kind = $1)`, kind)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Stats counts jobs by state, for metrics and operator visibility.
func (s *Store) Stats(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// truncErr bounds an error message so a pathological error string cannot bloat
// the row.
func truncErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const max = 2000
	if len(s) > max {
		return s[:max]
	}
	return s
}
