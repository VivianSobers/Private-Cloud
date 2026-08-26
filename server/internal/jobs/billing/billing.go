// Package billing is the metering and plan seam for Phase 9 slice 5.
//
// It is deliberately not a billing system. No payment provider is chosen, no
// invoice is produced, and no money moves. What it provides is the three things
// a billing integration would otherwise have to invent from scratch, none of
// which can be added retroactively:
//
//   - a PLAN, so "how much storage does this account get" is a named thing an
//     operator can change in one place instead of a bare number typed onto a
//     user row;
//   - a periodic METERING RECORD, so a billing period that has closed can still
//     be answered. Usage is a live measurement of bytes currently on disk, and
//     nothing recovers last March's figure once April has overwritten it;
//   - an outbound WEBHOOK, so whatever eventually charges for any of this can be
//     told, without this repository taking a dependency on it.
//
// The rule the whole package obeys: it introduces NO second accounting. Every
// byte it stores is copied from files.Usage — the numbers quota enforcement and
// GET /usage already answer from, down to the query text — because two notions
// of a number disagree eventually, and then nobody knows which to believe at the
// moment somebody is being charged for it. A plan does not shadow the quota
// either: assigning one WRITES THROUGH to users.quota_bytes, so there remains
// exactly one value enforcement reads.
package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors callers distinguish. Everything else is a server fault.
var (
	// ErrPlanNotFound is a plan id or name that names nothing.
	ErrPlanNotFound = errors.New("no such billing plan")
	// ErrAccountNotFound is a user id that names nothing.
	ErrAccountNotFound = errors.New("no such account")
	// ErrInvalidPlan is a plan whose fields cannot be stored as given.
	ErrInvalidPlan = errors.New("invalid billing plan")
)

// Plan is a named quota with optional price metadata.
//
// QuotaBytes nil means unlimited, matching users.quota_bytes exactly. It is the
// same absent-versus-null distinction Phase 7 had to get right on the user row,
// spelled the same way on purpose: one convention, not two.
type Plan struct {
	ID          uuid.UUID
	Name        string
	Description string
	QuotaBytes  *int64

	// Price metadata, carried and never interpreted. Minor units, never a float:
	// money in a float is a rounding error waiting for somebody to find it on a
	// statement. Currency is only meaningful beside an amount, which the schema
	// enforces rather than trusting.
	PriceCents *int32
	Currency   string
	Period     string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Assignment is the plan an account is on.
type Assignment struct {
	UserID     uuid.UUID
	Plan       *Plan
	AssignedAt time.Time
	AssignedBy *uuid.UUID
}

// Record is one owner's usage over one period.
type Record struct {
	ID      uuid.UUID
	OwnerID uuid.UUID
	Owner   string // username, joined for display; never the identity
	Start   time.Time
	End     time.Time

	LiveBytes    int64
	TrashBytes   int64
	VersionBytes int64
	FileCount    int64

	// PeakTotalBytes is the highest total any sample in this period saw.
	// Recorded beside the latest observation because WHICH of the two a bill
	// should be based on is a business decision this repository is not entitled
	// to make — and storing only one of them would make that decision silently,
	// now, by omission.
	PeakTotalBytes int64
	Samples        int

	PlanID     *uuid.UUID
	PlanName   string
	QuotaBytes *int64

	FirstSeenAt time.Time
	RecordedAt  time.Time
}

// TotalBytes is the figure a quota is measured against, computed here exactly as
// files.Usage computes it. Deliberately derived rather than stored: a stored
// total is a fourth number that can disagree with the three it is the sum of.
func (r Record) TotalBytes() int64 { return r.LiveBytes + r.TrashBytes + r.VersionBytes }

// Store reads and writes plans, assignments and metering records.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const planCols = `id, name, description, quota_bytes, price_cents, currency, period, created_at, updated_at`

func scanPlan(row pgx.Row) (*Plan, error) {
	var p Plan
	var currency *string
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &p.QuotaBytes,
		&p.PriceCents, &currency, &p.Period, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if currency != nil {
		p.Currency = *currency
	}
	return &p, nil
}

// ListPlans returns every plan, cheapest-sounding name order being useless, so
// alphabetical — the order an operator can predict.
func (s *Store) ListPlans(ctx context.Context) ([]*Plan, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+planCols+` FROM billing_plans ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPlan finds a plan by id.
func (s *Store) GetPlan(ctx context.Context, id uuid.UUID) (*Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx, `SELECT `+planCols+` FROM billing_plans WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	return p, err
}

// NormalisePlanName folds a plan name to its stored form.
//
// Case folding here rather than in the database, because "Free" and "free"
// arriving as two plans that render identically in a list is the kind of
// duplicate nobody notices until an account is on the wrong one. Same reasoning
// as nodes.name_fold, and the same decision to do it from migration one rather
// than reconcile real collisions later.
func NormalisePlanName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// UpsertPlan creates a plan or updates the one with that name.
//
// Upsert rather than separate create and update, because a plan is identified by
// its name to everybody who uses it — an operator re-running the same
// provisioning step should get the same plan, not a conflict.
//
// Changing a plan does NOT re-apply its quota to the accounts already on it.
// That is the load-bearing choice here: a fat-fingered edit to a plan would
// otherwise silently retighten every account on it at once, and the accounts
// affected would be exactly the ones nobody was looking at. Re-assignment is the
// explicit act — one account at a time, audited, and it says what it did.
func (s *Store) UpsertPlan(ctx context.Context, p Plan) (*Plan, error) {
	p.Name = NormalisePlanName(p.Name)
	if p.Name == "" {
		return nil, fmt.Errorf("%w: a plan needs a name", ErrInvalidPlan)
	}
	if len(p.Name) > 64 {
		return nil, fmt.Errorf("%w: name is longer than 64 characters", ErrInvalidPlan)
	}
	if p.QuotaBytes != nil && *p.QuotaBytes < 0 {
		return nil, fmt.Errorf("%w: quota_bytes cannot be negative", ErrInvalidPlan)
	}
	if p.PriceCents != nil && *p.PriceCents < 0 {
		return nil, fmt.Errorf("%w: price_cents cannot be negative", ErrInvalidPlan)
	}
	if p.PriceCents != nil && p.Currency == "" {
		return nil, fmt.Errorf("%w: a price needs a currency", ErrInvalidPlan)
	}
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if p.Currency != "" && len(p.Currency) != 3 {
		return nil, fmt.Errorf("%w: currency must be a three-letter ISO-4217 code", ErrInvalidPlan)
	}
	switch p.Period {
	case "":
		p.Period = "monthly"
	case "monthly", "yearly":
	default:
		return nil, fmt.Errorf("%w: period must be monthly or yearly", ErrInvalidPlan)
	}

	var currency *string
	if p.Currency != "" {
		currency = &p.Currency
	}

	return scanPlan(s.pool.QueryRow(ctx, `
		INSERT INTO billing_plans (name, description, quota_bytes, price_cents, currency, period)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (name) DO UPDATE SET
			description = EXCLUDED.description,
			quota_bytes = EXCLUDED.quota_bytes,
			price_cents = EXCLUDED.price_cents,
			currency    = EXCLUDED.currency,
			period      = EXCLUDED.period,
			updated_at  = now()
		RETURNING `+planCols,
		p.Name, p.Description, p.QuotaBytes, p.PriceCents, currency, p.Period))
}

// AssignPlan puts an account on a plan — or, with a nil planID, takes it off one.
//
// The whole point of the function is the second statement: the plan's quota is
// WRITTEN THROUGH to users.quota_bytes, in the same transaction, so the plan
// drives the quota that already exists rather than becoming a second gate beside
// it. Nothing in the upload path learns about plans, checkQuota is untouched,
// and there is still exactly one number enforcement reads.
//
// Detaching (planID nil) deliberately LEAVES the quota where the plan put it.
// Clearing it would silently grant unlimited storage to an account somebody was
// in the middle of removing from a plan, which is the opposite of what "take
// them off the paid tier" means. The quota stays and is editable directly, which
// is what makes the override case honest rather than magic.
func (s *Store) AssignPlan(ctx context.Context, userID uuid.UUID, planID *uuid.UUID, by *uuid.UUID) (*Assignment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM users WHERE id = $1`, userID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, err
	}

	if planID == nil {
		if _, err := tx.Exec(ctx, `DELETE FROM account_plans WHERE user_id = $1`, userID); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &Assignment{UserID: userID}, nil
	}

	plan, err := scanPlan(tx.QueryRow(ctx, `SELECT `+planCols+` FROM billing_plans WHERE id = $1`, *planID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPlanNotFound
	}
	if err != nil {
		return nil, err
	}

	var a Assignment
	a.UserID = userID
	a.Plan = plan
	if err := tx.QueryRow(ctx, `
		INSERT INTO account_plans (user_id, plan_id, assigned_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET
			plan_id     = EXCLUDED.plan_id,
			assigned_by = EXCLUDED.assigned_by,
			assigned_at = now()
		RETURNING assigned_at, assigned_by`,
		userID, plan.ID, by).Scan(&a.AssignedAt, &a.AssignedBy); err != nil {
		return nil, err
	}

	// The write-through. One statement, same transaction: an assignment that
	// committed without moving the quota would leave the console showing a plan
	// the enforcement path has never heard of.
	if _, err := tx.Exec(ctx, `UPDATE users SET quota_bytes = $2 WHERE id = $1`,
		userID, plan.QuotaBytes); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &a, nil
}

// Assignments returns every account's plan, keyed by user id.
//
// One query rather than one per account: the admin user list and the metering
// task both need the whole map, and N+1 over a user table is how a console that
// was fine with three accounts becomes slow with three hundred.
func (s *Store) Assignments(ctx context.Context) (map[uuid.UUID]*Assignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ap.user_id, ap.assigned_at, ap.assigned_by,
		       p.id, p.name, p.description, p.quota_bytes, p.price_cents,
		       p.currency, p.period, p.created_at, p.updated_at
		FROM account_plans ap JOIN billing_plans p ON p.id = ap.plan_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uuid.UUID]*Assignment{}
	for rows.Next() {
		var a Assignment
		var p Plan
		var currency *string
		if err := rows.Scan(&a.UserID, &a.AssignedAt, &a.AssignedBy,
			&p.ID, &p.Name, &p.Description, &p.QuotaBytes, &p.PriceCents, &currency,
			&p.Period, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if currency != nil {
			p.Currency = *currency
		}
		a.Plan = &p
		out[a.UserID] = &a
	}
	return out, rows.Err()
}

// RecordFilter narrows a read of the metering table.
type RecordFilter struct {
	OwnerID *uuid.UUID
	From    *time.Time
	To      *time.Time
	Limit   int
	Offset  int
}

const maxRecordLimit = 500

// ListRecords reads metering records newest period first.
func (s *Store) ListRecords(ctx context.Context, f RecordFilter) ([]*Record, error) {
	limit := f.Limit
	if limit <= 0 || limit > maxRecordLimit {
		limit = 100
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	rows, err := s.pool.Query(ctx, `
		SELECT m.id, m.owner_id, coalesce(u.username, ''),
		       m.period_start, m.period_end,
		       m.live_bytes, m.trash_bytes, m.version_bytes, m.file_count,
		       m.peak_total_bytes, m.samples,
		       m.plan_id, m.plan_name, m.quota_bytes,
		       m.first_seen_at, m.recorded_at
		FROM metering_records m
		LEFT JOIN users u ON u.id = m.owner_id
		WHERE ($1::uuid IS NULL OR m.owner_id = $1)
		  AND ($2::timestamptz IS NULL OR m.period_start >= $2)
		  AND ($3::timestamptz IS NULL OR m.period_start < $3)
		ORDER BY m.period_start DESC, u.username, m.owner_id
		LIMIT $4 OFFSET $5`,
		f.OwnerID, f.From, f.To, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.OwnerID, &r.Owner, &r.Start, &r.End,
			&r.LiveBytes, &r.TrashBytes, &r.VersionBytes, &r.FileCount,
			&r.PeakTotalBytes, &r.Samples,
			&r.PlanID, &r.PlanName, &r.QuotaBytes,
			&r.FirstSeenAt, &r.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// Snapshot is one measurement handed to the store: the numbers as files.Usage
// reported them, plus the plan the account was on when they were taken.
type Snapshot struct {
	OwnerID      uuid.UUID
	Period       Period
	LiveBytes    int64
	TrashBytes   int64
	VersionBytes int64
	FileCount    int64
	QuotaBytes   *int64
	PlanID       *uuid.UUID
	PlanName     string
}

// Total is the same sum files.Usage.TotalBytes computes.
func (s Snapshot) Total() int64 { return s.LiveBytes + s.TrashBytes + s.VersionBytes }

// RecordUsage writes a snapshot into the period's row, creating it if this is the
// period's first sample. It reports whether the row was created.
//
// The upsert is what makes the metering task safe to run at any cadence and to
// restart mid-sweep: a second run in the same period updates one row rather than
// inventing a second, contradictory measurement of it. The peak is a GREATEST
// against what is already stored, so it survives being sampled after a large
// delete — otherwise the highest figure an account ever reached would be
// whatever it happened to hold at the last tick of the month.
//
// samples is incremented rather than set, because the difference between "this
// account used nothing" and "nobody was measuring" is the first thing anybody
// asks when a period looks wrong months later, and a bare zero answers neither.
func (s *Store) RecordUsage(ctx context.Context, snap Snapshot) (bool, error) {
	var inserted bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO metering_records (
			owner_id, period_start, period_end,
			live_bytes, trash_bytes, version_bytes, file_count,
			peak_total_bytes, plan_id, plan_name, quota_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (owner_id, period_start) DO UPDATE SET
			live_bytes       = EXCLUDED.live_bytes,
			trash_bytes      = EXCLUDED.trash_bytes,
			version_bytes    = EXCLUDED.version_bytes,
			file_count       = EXCLUDED.file_count,
			peak_total_bytes = greatest(metering_records.peak_total_bytes, EXCLUDED.peak_total_bytes),
			plan_id          = EXCLUDED.plan_id,
			plan_name        = EXCLUDED.plan_name,
			quota_bytes      = EXCLUDED.quota_bytes,
			samples          = metering_records.samples + 1,
			recorded_at      = now()
		RETURNING (xmax = 0)`,
		snap.OwnerID, snap.Period.Start, snap.Period.End,
		snap.LiveBytes, snap.TrashBytes, snap.VersionBytes, snap.FileCount,
		snap.Total(), snap.PlanID, snap.PlanName, snap.QuotaBytes).Scan(&inserted)
	return inserted, err
}

// OwnerIDs lists every account the metering task should measure.
//
// Disabled accounts are included on purpose. A disabled account's files are
// still on the disk — DELETE /admin/users/{id} disables and revokes, it does not
// delete — so excluding them would make the metered total quietly stop matching
// what the pool holds, which is the one property a billing record has to keep.
func (s *Store) OwnerIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
