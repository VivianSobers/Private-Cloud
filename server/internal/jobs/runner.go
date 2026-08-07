package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"time"
)

// Handler performs one job. Returning an error reschedules the job with backoff
// until its attempts are spent, then dead-letters it; returning nil completes it.
type Handler func(ctx context.Context, j Job) error

// Runner drains the queue, dispatching each job to the handler registered for its
// kind. It is what `pcworker` runs — deliberately a small loop, because the whole
// point of the queue is that the interesting, memory-hungry work lives in the
// handlers, throttled to one at a time on this hardware.
type Runner struct {
	store    *Store
	handlers map[string]Handler
	log      *slog.Logger

	// idle is how long to wait after finding an empty queue before looking again;
	// lease is how long a claimed job may run before the reaper reclaims it.
	idle  time.Duration
	lease time.Duration
}

// Options tunes a Runner.
type Options struct {
	Idle  time.Duration
	Lease time.Duration
}

func NewRunner(store *Store, log *slog.Logger, opts Options) *Runner {
	if opts.Idle <= 0 {
		opts.Idle = 2 * time.Second
	}
	if opts.Lease <= 0 {
		opts.Lease = 10 * time.Minute
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runner{store: store, handlers: map[string]Handler{}, log: log, idle: opts.Idle, lease: opts.Lease}
}

// Register wires a handler for a job kind. Registering the same kind twice
// replaces the handler; a kind with no handler is simply never claimed.
func (r *Runner) Register(kind string, h Handler) { r.handlers[kind] = h }

// kinds is the set of registered kinds — the only ones the runner will claim, so
// a job whose handler this worker does not have is left for one that does.
func (r *Runner) kinds() []string {
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	return out
}

// Run drains the queue until the context is cancelled. A reaper runs alongside,
// returning jobs abandoned by a crashed worker to the queue. On cancellation the
// current job finishes and the loop exits — SIGTERM does not abandon work
// mid-step, it declines to start the next one.
func (r *Runner) Run(ctx context.Context) error {
	if len(r.handlers) == 0 {
		r.log.Warn("worker has no registered handlers; queue will not be drained")
	}

	reaper := time.NewTicker(r.lease / 2)
	defer reaper.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-reaper.C:
			if n, err := r.store.ReapStale(ctx, r.lease); err != nil {
				r.log.Warn("reap stale jobs failed", "error", err)
			} else if n > 0 {
				r.log.Warn("reclaimed stale jobs from a crashed worker", "count", n)
			}
			continue
		default:
		}

		worked, err := r.step(ctx)
		if err != nil {
			r.log.Warn("job step failed", "error", err)
			// An error here is the QUEUE failing, not a job failing — Fail or
			// Complete could not write. Treating that as work done skips the idle
			// wait, so a database outage becomes a hot loop hammering a database
			// that is already in trouble. Back off exactly as if the queue were
			// empty, which is operationally what it is.
			worked = false
		}
		if !worked {
			// Nothing to do: wait, but stay responsive to cancellation.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.idle):
			}
		}
	}
}

// step claims and runs one job, reporting whether it did any work. It is separate
// from Run so a test can drive one job deterministically.
func (r *Runner) step(ctx context.Context) (bool, error) {
	kinds := r.kinds()
	if len(kinds) == 0 {
		return false, nil
	}

	j, err := r.store.Claim(ctx, kinds)
	if errors.Is(err, ErrNoJob) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	h := r.handlers[j.Kind]
	if h == nil {
		// Claimed a kind we cannot handle — should not happen, since Claim filters
		// by registered kinds, but fail it rather than silently drop it.
		return true, r.store.Fail(ctx, j.ID, errors.New("no handler for kind "+j.Kind))
	}

	if herr := r.dispatch(ctx, h, j); herr != nil {
		r.log.Warn("job failed", "id", j.ID, "kind", j.Kind, "attempt", j.Attempts, "error", herr)
		return true, r.store.Fail(ctx, j.ID, herr)
	}
	r.log.Info("job done", "id", j.ID, "kind", j.Kind)
	return true, r.store.Complete(ctx, j.ID)
}

// dispatch runs a handler, turning a panic into an ordinary job failure.
//
// The API has withRecovery for exactly this reason; the worker had nothing, so
// one nil dereference in any handler killed the process. That is worse here than
// in the API: the claimed job stays 'running' until its lease expires, and with a
// restart policy in front, a poison-pill job becomes a crash loop that takes the
// whole lease-and-retry budget to work through. Failing the job instead lets
// backoff and dead-lettering do their job, and leaves the stack in the log.
func (r *Runner) dispatch(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("panic in job handler",
				"id", j.ID, "kind", j.Kind, "node", j.NodeID,
				"error", rec, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic in handler: %v", rec)
		}
	}()
	return h(ctx, j)
}
