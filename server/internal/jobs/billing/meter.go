package billing

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// PeriodKind is the calendar grain a billing period is cut on.
//
// Two, not a free-form duration. "Every 720 hours" drifts against the calendar
// and lands two runs a millisecond apart in different periods; a month is a
// calendar fact, and the only reason a day exists here is that a test cannot
// wait a month to observe a period close.
type PeriodKind string

const (
	PeriodMonthly PeriodKind = "month"
	PeriodDaily   PeriodKind = "day"
)

// ParsePeriodKind reads the configured grain, rejecting anything else rather
// than falling back — a typo silently metering by day instead of by month would
// produce thirty times the rows and look like it was working.
func ParsePeriodKind(s string) (PeriodKind, error) {
	switch PeriodKind(s) {
	case "", PeriodMonthly:
		return PeriodMonthly, nil
	case PeriodDaily:
		return PeriodDaily, nil
	default:
		return "", fmt.Errorf("billing period must be %q or %q (got %q)", PeriodMonthly, PeriodDaily, s)
	}
}

// Period is the half-open interval [Start, End) a snapshot belongs to.
type Period struct {
	Start time.Time
	End   time.Time
}

// Contains reports whether t falls in this period. Half-open, so the instant a
// period ends belongs to the next one and no measurement can be counted twice.
func (p Period) Contains(t time.Time) bool {
	u := t.UTC()
	return !u.Before(p.Start) && u.Before(p.End)
}

// PeriodFor returns the period t falls in.
//
// UTC, always. A local-time boundary moves twice a year, and the two failure
// modes it produces are a period that is an hour short and a period in which one
// hour happens twice — both of which would be discovered by somebody reading a
// bill rather than by anybody here.
func PeriodFor(t time.Time, kind PeriodKind) Period {
	u := t.UTC()
	switch kind {
	case PeriodDaily:
		start := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		return Period{Start: start, End: start.AddDate(0, 0, 1)}
	default:
		start := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		// AddDate(0, 1, 0) from the FIRST of a month is exact for every month:
		// the day-overflow normalisation that makes 31 January + 1 month land on
		// 2 or 3 March cannot bite, because the day is always 1.
		return Period{Start: start, End: start.AddDate(0, 1, 0)}
	}
}

// UsageSource is the one place metering gets its numbers, and it is
// deliberately the same *files.Store the quota check and GET /usage read.
//
// An interface rather than a concrete dependency so this package cannot grow a
// query of its own by accident: there is nothing here to write a second
// accounting WITH.
type UsageSource interface {
	Usage(ctx context.Context, ownerID uuid.UUID) (files.Usage, error)
}

// Meter takes usage snapshots on a cadence and emits billing events.
type Meter struct {
	store *Store
	usage UsageSource
	hook  *Webhook
	kind  PeriodKind
	log   *slog.Logger
	now   func() time.Time

	// announced remembers what has already been said once: which (owner, period)
	// produced a quota.exceeded event, and which period has been announced as
	// closed. Without it a full account emits one webhook per tick forever, which
	// is how an integration seam becomes a pager. In memory rather than a column,
	// because the consequence of forgetting on restart is one duplicate event —
	// and a receiver that cannot tolerate a duplicate delivery cannot tolerate
	// the retries either.
	announced map[string]bool
}

// NewMeter builds the metering task. hook may be nil, in which case usage is
// still recorded and nothing is delivered — which is the default deployment.
func NewMeter(store *Store, usage UsageSource, hook *Webhook, kind PeriodKind, log *slog.Logger) *Meter {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if kind == "" {
		kind = PeriodMonthly
	}
	return &Meter{
		store:     store,
		usage:     usage,
		hook:      hook,
		kind:      kind,
		log:       log,
		now:       time.Now,
		announced: map[string]bool{},
	}
}

// Run sweeps every interval until the context is cancelled.
//
// It sweeps ONCE IMMEDIATELY before waiting. A worker restarted on the first of
// the month would otherwise leave the new period with no row at all until the
// first tick, and a period with no row is indistinguishable from an account that
// stored nothing.
func (m *Meter) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if err := m.Sweep(ctx); err != nil && ctx.Err() == nil {
		m.log.Warn("metering sweep failed", "error", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.Sweep(ctx); err != nil && ctx.Err() == nil {
				m.log.Warn("metering sweep failed", "error", err)
			}
		}
	}
}

// Sweep writes one snapshot per owner for the current period, and closes any
// period that has ended since the last sweep.
//
// A failure on one owner does not abandon the others: the whole point of the
// record is that a period is answerable afterwards, and losing every account's
// month because one account's usage query failed is the worst available trade.
func (m *Meter) Sweep(ctx context.Context) error {
	now := m.now()
	period := PeriodFor(now, m.kind)

	owners, err := m.store.OwnerIDs(ctx)
	if err != nil {
		return fmt.Errorf("list owners: %w", err)
	}
	plans, err := m.store.Assignments(ctx)
	if err != nil {
		return fmt.Errorf("read plan assignments: %w", err)
	}

	// Closing the previous period is done before this one is written, so a
	// `period.closed` event is never emitted for a period the receiver has not
	// yet seen a final figure for.
	m.closePrevious(ctx, period)

	var firstErr error
	for _, owner := range owners {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u, err := m.usage.Usage(ctx, owner)
		if err != nil {
			m.log.Warn("metering: usage read failed; this owner has no sample for this tick",
				"owner", owner, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		snap := Snapshot{
			OwnerID: owner,
			Period:  period,
			// Copied, never recomputed. If these four lines ever grow arithmetic
			// of their own, this package has acquired the second accounting it
			// exists to avoid.
			LiveBytes:    u.LiveBytes,
			TrashBytes:   u.TrashBytes,
			VersionBytes: u.VersionBytes,
			FileCount:    u.FileCount,
			QuotaBytes:   u.QuotaBytes,
		}
		if a := plans[owner]; a != nil && a.Plan != nil {
			snap.PlanID = &a.Plan.ID
			snap.PlanName = a.Plan.Name
		}

		if _, err := m.store.RecordUsage(ctx, snap); err != nil {
			m.log.Warn("metering: could not record usage", "owner", owner, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		m.checkQuotaEvent(ctx, snap)
	}
	return firstErr
}

// checkQuotaEvent emits quota.exceeded the first time an account is over its
// limit in a period.
//
// Fired from the METERING sweep rather than from the 507 in the upload handler,
// and that is a decision rather than convenience. A hook on the refusal fires
// once per attempt — a sync client retrying a large file would deliver hundreds
// of identical events — and it would sit on the request path of the always-on
// box, which is the one place this codebase keeps clear of optional outbound
// work. What a billing integration actually needs to know is that an account IS
// over, which is a state, and a state is exactly what a periodic sweep observes.
func (m *Meter) checkQuotaEvent(ctx context.Context, snap Snapshot) {
	if snap.QuotaBytes == nil || snap.Total() <= *snap.QuotaBytes {
		return
	}
	key := snap.OwnerID.String() + "@" + snap.Period.Start.Format(time.RFC3339)
	if m.announced[key] {
		return
	}
	m.announced[key] = true
	m.hook.Notify(ctx, EventQuotaExceeded, map[string]any{
		"owner_id":     snap.OwnerID,
		"period_start": snap.Period.Start.Format(time.RFC3339),
		"period_end":   snap.Period.End.Format(time.RFC3339),
		"total_bytes":  snap.Total(),
		"quota_bytes":  *snap.QuotaBytes,
		"plan":         snap.PlanName,
	})
}

// closePrevious emits period.closed for the period immediately before the
// current one, once, when its final figures are already stored.
//
// "Once" is enforced by the sweep only doing it on the transition — the closed
// period's rows are already final, so re-announcing them on every tick for a
// month would be noise carrying no new information.
func (m *Meter) closePrevious(ctx context.Context, current Period) {
	if m.hook == nil || !m.hook.Enabled() {
		return
	}
	prev := PeriodFor(current.Start.Add(-time.Second), m.kind)
	key := "closed@" + prev.Start.Format(time.RFC3339)
	if m.announced[key] {
		return
	}

	records, err := m.store.ListRecords(ctx, RecordFilter{
		From: &prev.Start, To: &current.Start, Limit: maxRecordLimit,
	})
	if err != nil {
		m.log.Warn("metering: could not read the closing period", "error", err)
		return
	}
	if len(records) == 0 {
		// Nothing was measured in that period — a fresh deployment, or a worker
		// that was not running. Announcing a closed period with no data would
		// tell a receiver that every account stored nothing, which is a
		// confident wrong answer where silence is the honest one.
		m.announced[key] = true
		return
	}
	m.announced[key] = true

	accounts := make([]map[string]any, 0, len(records))
	for _, r := range records {
		entry := map[string]any{
			"owner_id":         r.OwnerID,
			"total_bytes":      r.TotalBytes(),
			"peak_total_bytes": r.PeakTotalBytes,
			"file_count":       r.FileCount,
			"samples":          r.Samples,
			"plan":             r.PlanName,
		}
		if r.QuotaBytes != nil {
			entry["quota_bytes"] = *r.QuotaBytes
		}
		accounts = append(accounts, entry)
	}
	m.hook.Notify(ctx, EventPeriodClosed, map[string]any{
		"period_start": prev.Start.Format(time.RFC3339),
		"period_end":   prev.End.Format(time.RFC3339),
		"accounts":     accounts,
	})
}
