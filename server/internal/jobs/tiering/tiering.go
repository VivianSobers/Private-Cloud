// Package tiering demotes cold content to object storage and promotes it back.
//
// It is a job kind like extract, media and embed, and for the same reason they
// are: the work is unbounded, IO-heavy and must never run inside a request. The
// difference is what a failure costs. A failed extraction means a file is not
// searchable by its text; a failed demotion that had already deleted the local
// copy means the file is GONE. Every ordering decision below exists to make
// that impossible, and the shape of it is one sentence long:
//
//	nothing local is deleted until the cold copy has been read back and matched.
//
// The policy itself is deliberately dull — age, idleness, size — because a
// clever policy is one whose mistakes are hard to predict, and its mistakes here
// are measured in how long somebody waits for their own photographs.
package tiering

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
)

// Kind is the job kind this handler drains. Its own kind, not a phase of the
// media or extract job, so a deployment can run tiering on the box that can
// reach the bucket and nothing else.
const Kind = "tier"

// Policy is what decides that content is cold.
//
// All three tests must pass. Age alone would demote a large file uploaded last
// month and read every day; idleness alone would demote a file uploaded an hour
// ago that nobody has opened yet, which is every upload. Size is the third
// because moving a 4 KiB blob to object storage costs a request per read
// forever to reclaim four kilobytes.
type Policy struct {
	// MinAge is how old the content must be, measured from creation.
	MinAge time.Duration
	// MinIdle is how long since it was last read. Content with no recorded
	// access falls back to its creation time — see migration 00028 for why that
	// is not backfilled to now().
	MinIdle time.Duration
	// MinSize is the smallest object worth moving, in bytes.
	MinSize int64
	// Batch bounds one sweep, so a single run cannot spend the night saturating
	// a domestic uplink and cannot hold a transaction open across all of it.
	Batch int
}

// DefaultPolicy is conservative on every axis. An operator turning the cold
// tier on for the first time should find that almost nothing qualifies, and
// then loosen it deliberately — the opposite mistake drains the pool onto a
// link that was never tested at that volume.
func DefaultPolicy() Policy {
	return Policy{
		MinAge:  90 * 24 * time.Hour,
		MinIdle: 90 * 24 * time.Hour,
		MinSize: 1 << 20,
		Batch:   100,
	}
}

func (p Policy) validate() error {
	if p.MinAge <= 0 {
		return errors.New("PC_COLD_TIER_MIN_AGE must be positive")
	}
	if p.MinIdle <= 0 {
		return errors.New("PC_COLD_TIER_MIN_IDLE must be positive")
	}
	if p.MinSize < 0 {
		return errors.New("PC_COLD_TIER_MIN_SIZE cannot be negative")
	}
	if p.Batch <= 0 {
		return errors.New("PC_COLD_TIER_BATCH must be positive")
	}
	return nil
}

// Candidate is one demotable object.
type Candidate struct {
	// Table is "blobs" or "chunks". Carried explicitly rather than inferred from
	// the key, because the two share one key space by design and guessing wrong
	// would mark the wrong row.
	Table      string
	StorageKey string
	// Size is what is on disk: `size` for a whole-file blob, `stored_size` for a
	// chunk, which is the compressed form. Reporting the logical size would
	// overstate what the demotion reclaimed.
	Size int64
}

// Result reports one sweep.
type Result struct {
	Considered int
	Demoted    int
	BytesMoved int64
	Failed     int
	// FirstError is kept so the job's last_error says something specific. The
	// sweep does not stop at the first failure: one unreadable blob must not
	// prevent the other ninety-nine from moving.
	FirstError error
}

// Handler runs the policy. It is what pcworker registers for Kind and what
// `cloudctl tier run` calls directly.
type Handler struct {
	store  *Store
	blobs  *blob.TieredStore
	policy Policy
	log    *slog.Logger
}

func NewHandler(store *Store, blobs *blob.TieredStore, policy Policy, log *slog.Logger) (*Handler, error) {
	if blobs == nil || !blobs.Enabled() {
		return nil, errors.New("tiering needs a cold tier; set PC_COLD_TIER_ENABLED and the S3 settings")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Handler{store: store, blobs: blobs, policy: policy, log: log}, nil
}

// Policy reports the policy in force, for `cloudctl tier status` and the admin
// storage report. A tiering run that will not say what rule it applies is one
// nobody can predict.
func (h *Handler) Policy() Policy { return h.policy }

// Sweep demotes one batch and returns what it did.
//
// Errors on individual objects are counted, not returned: the caller is a job
// whose failure reschedules the WHOLE batch, and one blob that cannot be read
// would then block every other candidate behind it forever. A sweep returns an
// error only when it could not talk to the database at all.
func (h *Handler) Sweep(ctx context.Context) (Result, error) {
	var res Result

	candidates, err := h.store.Candidates(ctx, h.policy)
	if err != nil {
		return res, err
	}
	res.Considered = len(candidates)

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			// A worker shutting down stops starting new demotions. The one in
			// flight has already finished or failed; neither leaves a half state.
			return res, nil
		}

		moved, err := h.blobs.Demote(ctx, c.StorageKey)
		if err != nil {
			res.Failed++
			if res.FirstError == nil {
				res.FirstError = err
			}
			h.log.Warn("demotion failed; the local copy was left alone",
				"table", c.Table, "key", c.StorageKey, "error", err)
			continue
		}

		// The row is flipped AFTER the bytes moved, so a crash in between leaves
		// a row saying 'hot' whose bytes are only cold. That direction is safe:
		// the tiered store falls through to the bucket on a local miss whatever
		// the row says, and fsck confirms the cold tier before it calls anything
		// missing. The reverse order — row first — would be a row saying 'cold'
		// for bytes still only on local disk, which is the state that makes a
		// later "it is cold, do not worry" a lie.
		if err := h.store.MarkCold(ctx, c.Table, c.StorageKey); err != nil {
			res.Failed++
			if res.FirstError == nil {
				res.FirstError = err
			}
			h.log.Error("bytes are in the cold tier but the row still says hot; fsck will reconcile",
				"table", c.Table, "key", c.StorageKey, "error", err)
			continue
		}

		res.Demoted++
		res.BytesMoved += moved
	}

	if res.Demoted > 0 || res.Failed > 0 {
		h.log.Info("tiering sweep finished",
			"considered", res.Considered, "demoted", res.Demoted,
			"bytes", res.BytesMoved, "failed", res.Failed)
	}
	return res, nil
}

// Handle adapts Sweep to the jobs.Handler signature.
//
// The job carries a node id and an owner like every other, and this handler
// ignores both: tiering is a property of storage, not of one person's file. It
// is registered as a kind anyway so the queue's claim, lease, backoff and
// dead-lettering apply to it exactly as they do to OCR — a sweep that cannot
// reach the bucket should back off and eventually be visible in
// `cloudctl jobs failed`, not silently retry at wire speed.
func (h *Handler) Handle(ctx context.Context) error {
	res, err := h.Sweep(ctx)
	if err != nil {
		return err
	}
	if res.Failed > 0 && res.Demoted == 0 {
		// Nothing moved and something broke: worth retrying with backoff, and
		// worth dead-lettering if it keeps happening. A partially successful
		// sweep is not a failure — the next sweep picks up what is left.
		return fmt.Errorf("tiering demoted nothing and failed %d time(s): %w", res.Failed, res.FirstError)
	}
	return nil
}

// Restore pulls content back to the local pool, blocking until it is there.
//
// The operator-facing counterpart to the read path's promote-on-access, for
// `cloudctl tier restore`. It blocks rather than returning "in progress"
// because a person at a terminal who typed `restore` wants the answer, not a
// status code — the 202 contract exists for HTTP clients that cannot wait.
func (h *Handler) Restore(ctx context.Context, keys []string) (int, error) {
	restored := 0
	for _, key := range keys {
		// A generous per-key deadline instead of the store's RestoreWait: this
		// path is explicitly allowed to take as long as the transfer takes.
		if err := h.blobs.Warm(ctx, key); err != nil {
			if errors.Is(err, blob.ErrRestoring) {
				// Started, still running. Wait it out rather than reporting a
				// half-answer to a command that was asked to restore.
				for errors.Is(err, blob.ErrRestoring) {
					select {
					case <-ctx.Done():
						return restored, ctx.Err()
					case <-time.After(2 * time.Second):
					}
					err = h.blobs.Warm(ctx, key)
				}
			}
			if err != nil {
				return restored, fmt.Errorf("restore %s: %w", key, err)
			}
		}
		if err := h.store.MarkHot(ctx, key); err != nil {
			return restored, err
		}
		restored++
	}
	return restored, nil
}

// --- the database side ------------------------------------------------------

// Store is the tier column's half of the schema. It is separate from
// files.Store and cas.Store on purpose: both of those own their table's
// lifecycle, and neither should grow a second reason to know that object
// storage exists.
type Store struct {
	pool *pgxpool.Pool

	// touched rate-limits the access-recency write. Reads are the hottest path
	// in the system and a database UPDATE per range request would cost far more
	// than the tiering ever saves, so a key's access time is written at most
	// once per touchInterval per process.
	mu      sync.Mutex
	touched map[string]time.Time
}

// touchInterval is how often one key's last_access_at is actually written. An
// hour is far finer than any policy that measures idleness in days, and coarse
// enough that a video being scrubbed costs one write, not one per seek.
const touchInterval = time.Hour

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, touched: map[string]time.Time{}}
}

// Candidates lists what the policy says should move, oldest access first.
//
// refcount > 0 is not an optimisation. A row at zero references is what the GC
// is about to delete, and paying to upload it to object storage first — then
// paying again to delete it there — is work whose only outcome is a bill.
func (s *Store) Candidates(ctx context.Context, p Policy) ([]Candidate, error) {
	out := make([]Candidate, 0, p.Batch)

	// Two queries rather than a UNION: the size column differs (chunks are
	// stored compressed, so `stored_size` is what is on disk), the batch is
	// meant to be shared between them, and a UNION would hide which table a row
	// came from at exactly the moment that matters for the UPDATE afterwards.
	rows, err := s.pool.Query(ctx, `
		SELECT storage_key, size FROM blobs
		WHERE tier = 'hot'
		  AND refcount > 0
		  AND created_at < now() - $1::interval
		  AND coalesce(last_access_at, created_at) < now() - $2::interval
		  AND size >= $3
		ORDER BY coalesce(last_access_at, created_at)
		LIMIT $4`,
		p.MinAge.String(), p.MinIdle.String(), p.MinSize, p.Batch)
	if err != nil {
		return nil, fmt.Errorf("blob demotion candidates: %w", err)
	}
	for rows.Next() {
		c := Candidate{Table: "blobs"}
		if err := rows.Scan(&c.StorageKey, &c.Size); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	remaining := p.Batch - len(out)
	if remaining <= 0 {
		return out, nil
	}

	rows, err = s.pool.Query(ctx, `
		SELECT storage_key, stored_size FROM chunks
		WHERE tier = 'hot'
		  AND refcount > 0
		  AND created_at < now() - $1::interval
		  AND coalesce(last_access_at, created_at) < now() - $2::interval
		  AND stored_size >= $3
		ORDER BY coalesce(last_access_at, created_at)
		LIMIT $4`,
		p.MinAge.String(), p.MinIdle.String(), p.MinSize, remaining)
	if err != nil {
		return nil, fmt.Errorf("chunk demotion candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		c := Candidate{Table: "chunks"}
		var stored int
		if err := rows.Scan(&c.StorageKey, &stored); err != nil {
			return nil, err
		}
		c.Size = int64(stored)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkCold records that a key's bytes have moved. table is checked against a
// literal allowlist rather than interpolated, because a table name cannot be a
// bind parameter and this is the one place in the package where that is true.
func (s *Store) MarkCold(ctx context.Context, table, key string) error {
	var sql string
	switch table {
	case "blobs":
		sql = `UPDATE blobs SET tier = 'cold', tiered_at = now() WHERE storage_key = $1`
	case "chunks":
		sql = `UPDATE chunks SET tier = 'cold', tiered_at = now() WHERE storage_key = $1`
	default:
		return fmt.Errorf("unknown storage table %q", table)
	}
	_, err := s.pool.Exec(ctx, sql, key)
	return err
}

// MarkHot records a promotion. Both tables are updated because the caller — the
// blob store — genuinely does not know which one a key belongs to, and should
// not: the two share a key space precisely so that a tier move is the same
// operation for both.
func (s *Store) MarkHot(ctx context.Context, key string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE blobs SET tier = 'hot', tiered_at = now(), last_access_at = now()
		 WHERE storage_key = $1 AND tier <> 'hot'`, key); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE chunks SET tier = 'hot', tiered_at = now(), last_access_at = now()
		 WHERE storage_key = $1 AND tier <> 'hot'`, key)
	return err
}

// Touch records that a key was read. Best effort by contract: it is called from
// the download path, and a failure to note an access must never fail a
// download. The worst consequence of losing one is that content looks idler
// than it is and is demoted a little early — which the promote-on-read path
// then undoes.
func (s *Store) Touch(ctx context.Context, key string) {
	s.mu.Lock()
	if last, ok := s.touched[key]; ok && time.Since(last) < touchInterval {
		s.mu.Unlock()
		return
	}
	s.touched[key] = time.Now()
	// Bound the map. It is a rate-limiting cache, not a record of anything, so
	// dropping all of it costs at most one redundant UPDATE per key afterwards.
	if len(s.touched) > 50000 {
		s.touched = map[string]time.Time{key: time.Now()}
	}
	s.mu.Unlock()

	// Detached from the request context: a client that disconnects the instant
	// after its read must not leave the access unrecorded, and this write is
	// milliseconds.
	go func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = s.pool.Exec(c, `UPDATE blobs SET last_access_at = now() WHERE storage_key = $1`, key)
		_, _ = s.pool.Exec(c, `UPDATE chunks SET last_access_at = now() WHERE storage_key = $1`, key)
	}()
}

// Usage is the cold-tier accounting behind GET /admin/storage.
type Usage struct {
	ColdBlobs      int64
	ColdBlobBytes  int64
	ColdChunks     int64
	ColdChunkBytes int64
}

// Bytes totals what the cold tier holds, as the database accounts for it.
// Deliberately not the bucket's own reported size: this is what the application
// believes it put there, and the difference between the two is exactly the
// discrepancy fsck exists to find.
func (u Usage) Bytes() int64 { return u.ColdBlobBytes + u.ColdChunkBytes }

// Objects totals cold rows across both tables.
func (u Usage) Objects() int64 { return u.ColdBlobs + u.ColdChunks }

func (s *Store) Usage(ctx context.Context) (Usage, error) {
	var u Usage
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(size), 0) FROM blobs WHERE tier = 'cold'`).
		Scan(&u.ColdBlobs, &u.ColdBlobBytes); err != nil {
		return Usage{}, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(stored_size), 0) FROM chunks WHERE tier = 'cold'`).
		Scan(&u.ColdChunks, &u.ColdChunkBytes); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// ColdKeys lists every key the database says is cold — what `cloudctl tier
// restore --all` walks, and what fsck compares the bucket against.
func (s *Store) ColdKeys(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT storage_key FROM blobs WHERE tier = 'cold'
		UNION ALL
		SELECT storage_key FROM chunks WHERE tier = 'cold'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
