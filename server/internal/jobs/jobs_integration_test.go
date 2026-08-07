package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Integration tests against a real Postgres. Each test uses a unique job kind so
// it is isolated from other tests (and other packages) sharing the database:
// Claim filters by kind, so one test's worker never takes another's jobs.
//
//	PC_TEST_DATABASE_URL=postgres://... go test ./internal/jobs/...

type jobsFixture struct {
	store  *Store
	pool   *pgxpool.Pool
	owner  uuid.UUID
	nodeID uuid.UUID
	kind   string
}

func newJobsFixture(t *testing.T) *jobsFixture {
	t.Helper()
	dsn := os.Getenv("PC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PC_TEST_DATABASE_URL not set; skipping jobs integration tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	database, err := db.Open(ctx, dsn, 8, 1, 10*time.Second, log)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(database.Close)
	if err := database.Migrate(ctx, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	name := "job-" + uuid.NewString()[:8]
	user, err := auth.NewStore(database.Pool).CreateUser(ctx, uuid.New(), name, name, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	root, err := files.NewStore(database.Pool).EnsureRoot(ctx, user.ID)
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}

	return &jobsFixture{
		store:  NewStore(database.Pool),
		pool:   database.Pool,
		owner:  user.ID,
		nodeID: root.ID,
		kind:   "test-" + uuid.NewString(),
	}
}

func (f *jobsFixture) countByState(t *testing.T, state string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE owner_id = $1 AND state = $2`, f.owner, state).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", state, err)
	}
	return n
}

func (f *jobsFixture) stateOf(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT state FROM jobs WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("state of %s: %v", id, err)
	}
	return s
}

func TestEnqueueAndClaim(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	id, created, err := f.store.Enqueue(ctx, f.kind, &f.nodeID, f.owner, EnqueueOptions{})
	if err != nil || !created {
		t.Fatalf("enqueue: created=%v err=%v", created, err)
	}

	j, err := f.store.Claim(ctx, []string{f.kind})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if j.ID != id || j.State != StateRunning || j.Attempts != 1 {
		t.Errorf("claimed job wrong: %+v", j)
	}
	if j.NodeID == nil || *j.NodeID != f.nodeID {
		t.Errorf("claimed job lost its node id")
	}

	// Nothing left to claim.
	if _, err := f.store.Claim(ctx, []string{f.kind}); !errors.Is(err, ErrNoJob) {
		t.Errorf("second claim = %v, want ErrNoJob", err)
	}

	if err := f.store.Complete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if f.countByState(t, StateDone) != 1 {
		t.Error("job not marked done")
	}
}

// A pending job for the same (kind, node) is not duplicated: a burst of edits to
// one file does not queue a burst of identical work.
func TestEnqueueDeduplicatesPending(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	if _, created, err := f.store.Enqueue(ctx, f.kind, &f.nodeID, f.owner, EnqueueOptions{}); err != nil || !created {
		t.Fatalf("first enqueue: created=%v err=%v", created, err)
	}
	_, created, err := f.store.Enqueue(ctx, f.kind, &f.nodeID, f.owner, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second enqueue created a duplicate pending job")
	}
	if f.countByState(t, StateQueued) != 1 {
		t.Errorf("expected exactly one queued job, got %d", f.countByState(t, StateQueued))
	}
}

// Two workers claiming at once never take the same job — the SKIP LOCKED
// property, tested under real concurrency.
func TestClaimSkipsLockedUnderConcurrency(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	const total = 20
	for i := 0; i < total; i++ {
		// Owner-scoped jobs (nil node) so the unique-pending index does not collapse
		// them, giving a genuine queue of distinct jobs to race over.
		if _, _, err := f.store.Enqueue(ctx, f.kind, nil, f.owner, EnqueueOptions{}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	var (
		mu      sync.Mutex
		claimed = map[uuid.UUID]int{}
	)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := f.store.Claim(ctx, []string{f.kind})
				if errors.Is(err, ErrNoJob) {
					return
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				mu.Lock()
				claimed[j.ID]++
				mu.Unlock()
				// Complete each so the run does not leave 'running' rows behind in
				// the shared database.
				if err := f.store.Complete(ctx, j.ID); err != nil {
					t.Errorf("complete: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(claimed) != total {
		t.Errorf("claimed %d distinct jobs, want %d", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("job %s claimed %d times", id, n)
		}
	}
}

// A failing job is rescheduled with backoff until its attempts are spent, then
// dead-lettered — never lost, never looping at wire speed.
func TestFailRetriesThenDeadLetters(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	id, _, err := f.store.Enqueue(ctx, f.kind, &f.nodeID, f.owner, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Shorten the budget in SQL rather than through an option only this test
	// would ever pass — the same way the test already advances run_after below.
	if _, err := f.pool.Exec(ctx, `UPDATE jobs SET max_attempts = 2 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	// First failure: attempts 1 of 2, so it goes back to the queue for later.
	if _, err := f.store.Claim(ctx, []string{f.kind}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Fail(ctx, id, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if f.countByState(t, StateQueued) != 1 {
		t.Fatalf("job not requeued after first failure")
	}

	// Its backoff pushed run_after into the future; make it eligible again.
	if _, err := f.pool.Exec(ctx, `UPDATE jobs SET run_after = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	// Second failure: attempts 2 of 2, so it dead-letters.
	if _, err := f.store.Claim(ctx, []string{f.kind}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Fail(ctx, id, errors.New("boom again")); err != nil {
		t.Fatal(err)
	}
	if f.countByState(t, StateFailed) != 1 {
		t.Errorf("job did not dead-letter after exhausting attempts")
	}

	var lastErr string
	if err := f.pool.QueryRow(ctx, `SELECT last_error FROM jobs WHERE id = $1`, id).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if lastErr != "boom again" {
		t.Errorf("last_error = %q, want the most recent cause", lastErr)
	}
}

// A job left 'running' by a crashed worker is returned to the queue once its
// lease expires, and a freshly-claimed one is not.
func TestReapStaleRequeues(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	id, _, err := f.store.Enqueue(ctx, f.kind, &f.nodeID, f.owner, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Claim(ctx, []string{f.kind}); err != nil {
		t.Fatal(err)
	}

	// ReapStale is global by design — the worker reaps every abandoned job, not
	// just one owner's — so this scopes its assertions to this test's own job by
	// id rather than to a global count the shared database would make flaky.

	// A fresh claim is within its lease and must not be reaped.
	if _, err := f.store.ReapStale(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if s := f.stateOf(t, id); s != StateRunning {
		t.Fatalf("fresh running job was reaped: state=%s", s)
	}

	// Age the claim past the lease; now it must be reclaimed.
	if _, err := f.pool.Exec(ctx, `UPDATE jobs SET claimed_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReapStale(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if s := f.stateOf(t, id); s != StateQueued {
		t.Errorf("stale job did not return to the queue: state=%s", s)
	}
}

// Failed jobs are listable and can be requeued in bulk — the operator's recovery
// path after fixing whatever made them fail.
func TestListAndRetryFailed(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	id, _, err := f.store.Enqueue(ctx, f.kind, &f.nodeID, f.owner, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// One attempt, so the first failure dead-letters immediately.
	if _, err := f.pool.Exec(ctx, `UPDATE jobs SET max_attempts = 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Claim(ctx, []string{f.kind}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Fail(ctx, id, errors.New("kaboom")); err != nil {
		t.Fatal(err)
	}

	failed, err := f.store.ListFailed(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, j := range failed {
		if j.ID == id {
			found = true
			if j.LastError != "kaboom" {
				t.Errorf("last_error = %q, want kaboom", j.LastError)
			}
		}
	}
	if !found {
		t.Fatal("failed job not listed")
	}

	// A filter naming a different kind must leave this job dead-lettered.
	if n, err := f.store.RetryFailed(ctx, "some-other-kind"); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("retry of an unrelated kind requeued %d job(s), want 0", n)
	}
	if s := f.stateOf(t, id); s != StateFailed {
		t.Errorf("job state after filtered retry = %s, want failed", s)
	}

	// An empty filter is the explicit "retry everything".
	if _, err := f.store.RetryFailed(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if s := f.stateOf(t, id); s != StateQueued {
		t.Errorf("retried job state = %s, want queued", s)
	}
}

// The runner claims a job, runs its handler, and completes it.
func TestRunnerProcessesJob(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	var got uuid.UUID
	runner := NewRunner(f.store, nil, Options{})
	runner.Register(f.kind, func(_ context.Context, j Job) error {
		got = j.ID
		return nil
	})

	id, _, err := f.store.Enqueue(ctx, f.kind, &f.nodeID, f.owner, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	worked, err := runner.step(ctx)
	if err != nil || !worked {
		t.Fatalf("step: worked=%v err=%v", worked, err)
	}
	if got != id {
		t.Errorf("handler ran for %s, want %s", got, id)
	}
	if f.countByState(t, StateDone) != 1 {
		t.Error("job not completed by runner")
	}
	// Queue now empty for this kind.
	if worked, _ := runner.step(ctx); worked {
		t.Error("runner claimed a job from an empty queue")
	}
}

// A handler that errors drives the job through the failure path.
func TestRunnerFailingHandlerDeadLetters(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()

	runner := NewRunner(f.store, nil, Options{})
	runner.Register(f.kind, func(context.Context, Job) error {
		return errors.New("handler exploded")
	})

	id, _, err := f.store.Enqueue(ctx, f.kind, &f.nodeID, f.owner, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// One attempt, so a single failing step is enough to dead-letter it.
	if _, err := f.pool.Exec(ctx, `UPDATE jobs SET max_attempts = 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.step(ctx); err != nil || !worked {
		t.Fatalf("step: worked=%v err=%v", worked, err)
	}
	if f.countByState(t, StateFailed) != 1 {
		t.Error("failing handler did not dead-letter the job")
	}
}
