package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs/billing"
)

// Phase 9 slice 5: the billing HOOKS surface.
//
// Four admin-only endpoints and one outbound webhook. What they deliberately are
// not is a billing system: nothing here prices anything, produces an invoice or
// talks to a payment provider, because no provider has been chosen and the thing
// billing would attach to is one person's disk. What they are is the seam a
// provider integration would otherwise have to invent — and the two halves of
// that seam which cannot be added retroactively, since a period that was never
// measured cannot be measured again afterwards.
//
// The rule the surface obeys, and the reason it can be trusted: a PLAN DRIVES
// THE EXISTING QUOTA, it does not shadow it. Assigning a plan writes the plan's
// quota through to users.quota_bytes inside one transaction, so checkQuota is
// untouched, the upload path never learns what a plan is, and there is still
// exactly one number enforcement reads. Two notions of a number disagree
// eventually, and a disagreement about how full an account is would be found by
// somebody being refused an upload while the console told them they had room.

// billingStore builds the store lazily from the server's pool.
//
// Lazily rather than as a field on Server, because Server is constructed in the
// contract test with a nil database to enumerate routes, and a store built
// eagerly there would be a store over a nil pool. The handlers below all check
// for that state and report the feature unavailable, which is also the honest
// answer on a server whose migrations have not run.
func (s *Server) billingStore() *billing.Store {
	if s.db == nil {
		return nil
	}
	return billing.NewStore(s.db.Pool)
}

// SetBillingWebhook wires the outbound hook. Left unset when no endpoint is
// configured, which is the default and a fully supported state: plans and
// metering work identically, and nothing is delivered anywhere.
func (s *Server) SetBillingWebhook(w *billing.Webhook) { s.billingHook = w }

// billingUnavailable reports the one state these handlers cannot serve from.
func (s *Server) billingUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.billingStore() != nil {
		return false
	}
	writeError(w, r, http.StatusServiceUnavailable, "billing_unavailable",
		"billing is not available on this server")
	return true
}

func planJSON(p *billing.Plan) map[string]any {
	out := map[string]any{
		"id":          p.ID,
		"name":        p.Name,
		"description": p.Description,
		"period":      p.Period,
		"created_at":  p.CreatedAt,
		"updated_at":  p.UpdatedAt,
	}
	// Absent means unlimited, exactly as it does on a user. Sending 0 would be a
	// plan of zero bytes, which is the opposite instruction — the same trap the
	// admin user JSON documents, and it is spelled the same way here so a client
	// needs one rule rather than two.
	if p.QuotaBytes != nil {
		out["quota_bytes"] = *p.QuotaBytes
	}
	if p.PriceCents != nil {
		out["price_cents"] = *p.PriceCents
		out["currency"] = p.Currency
	}
	return out
}

func (s *Server) handleAdminListPlans(w http.ResponseWriter, r *http.Request) {
	if s.billingUnavailable(w, r) {
		return
	}
	plans, err := s.billingStore().ListPlans(r.Context())
	if err != nil {
		s.serverError(w, r, "list billing plans", err)
		return
	}
	out := make([]map[string]any, 0, len(plans))
	for _, p := range plans {
		out = append(out, planJSON(p))
	}

	// The assignment count rides along, because the question an operator asks
	// immediately after "what plans are there" is "is anybody on this one" —
	// which is also the question that decides whether a plan can be retired.
	counts := map[string]int{}
	if assignments, err := s.billingStore().Assignments(r.Context()); err == nil {
		for _, a := range assignments {
			if a.Plan != nil {
				counts[a.Plan.ID.String()]++
			}
		}
		for _, entry := range out {
			entry["account_count"] = counts[entry["id"].(uuid.UUID).String()]
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"plans": out})
}

// handleAdminUpsertPlan creates a plan, or updates the one with that name.
//
// Upsert rather than create-then-conflict because a plan is identified by its
// name to everyone who uses it: re-running the same provisioning step should
// yield the same plan rather than a 409 an operator has to work around.
//
// It deliberately does NOT re-apply the new quota to accounts already on the
// plan. A mistyped edit would otherwise retighten every one of them at once, and
// the accounts affected would be precisely the ones nobody was looking at.
// Re-assignment is the explicit, audited, one-account-at-a-time act.
func (s *Server) handleAdminUpsertPlan(w http.ResponseWriter, r *http.Request) {
	if s.billingUnavailable(w, r) {
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		QuotaBytes  *int64 `json:"quota_bytes"`
		PriceCents  *int32 `json:"price_cents"`
		Currency    string `json:"currency"`
		Period      string `json:"period"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	plan, err := s.billingStore().UpsertPlan(r.Context(), billing.Plan{
		Name:        body.Name,
		Description: body.Description,
		QuotaBytes:  body.QuotaBytes,
		PriceCents:  body.PriceCents,
		Currency:    body.Currency,
		Period:      body.Period,
	})
	if errors.Is(err, billing.ErrInvalidPlan) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err != nil {
		s.serverError(w, r, "upsert billing plan", err)
		return
	}

	s.audit(r, "billing.plan.upsert", plan.Name, map[string]any{
		"quota_bytes": plan.QuotaBytes, "price_cents": plan.PriceCents,
	})
	writeJSON(w, r, http.StatusOK, map[string]any{"plan": planJSON(plan)})
}

// handleAdminSetAccountPlan puts an account on a plan, or takes it off one.
//
// This is the endpoint the whole slice turns on: the plan's quota is written
// through to users.quota_bytes in the same transaction, so the plan drives
// enforcement that already exists rather than becoming a second gate beside it.
//
// A null plan_id detaches and LEAVES the quota where the plan put it. Clearing
// it would silently hand unlimited storage to an account somebody was in the
// middle of removing from a paid tier, which is the opposite of what the action
// means. The quota stays and remains directly editable, which is what keeps the
// override case honest rather than magic.
func (s *Server) handleAdminSetAccountPlan(w http.ResponseWriter, r *http.Request) {
	if s.billingUnavailable(w, r) {
		return
	}
	userID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid user id")
		return
	}

	var body struct {
		PlanID *string `json:"plan_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	var planID *uuid.UUID
	if body.PlanID != nil && *body.PlanID != "" {
		parsed, err := uuid.Parse(*body.PlanID)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid plan id")
			return
		}
		planID = &parsed
	}

	assignment, err := s.billingStore().AssignPlan(r.Context(), userID, planID, &CurrentUser(r.Context()).ID)
	switch {
	case errors.Is(err, billing.ErrAccountNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such user")
		return
	case errors.Is(err, billing.ErrPlanNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such billing plan")
		return
	case err != nil:
		s.serverError(w, r, "assign billing plan", err)
		return
	}

	out := map[string]any{"user_id": userID}
	detail := map[string]any{}
	if assignment.Plan != nil {
		out["plan"] = planJSON(assignment.Plan)
		out["assigned_at"] = assignment.AssignedAt
		detail["plan"] = assignment.Plan.Name
		if assignment.Plan.QuotaBytes != nil {
			detail["quota_bytes"] = *assignment.Plan.QuotaBytes
		}
	}

	// Audited like every other admin action, and audited BEFORE the webhook,
	// because the audit log is this server's own record and the webhook is
	// somebody else's. If only one of them survives, it should be ours.
	s.audit(r, "billing.plan.assign", userID.String(), detail)
	s.billingHook.Notify(r.Context(), billing.EventPlanChanged, map[string]any{
		"user_id": userID,
		"plan":    detail["plan"],
		"quota_bytes": func() any {
			if assignment.Plan != nil {
				return assignment.Plan.QuotaBytes
			}
			return nil
		}(),
	})
	writeJSON(w, r, http.StatusOK, out)
}

// handleAdminMetering reads the metering records.
//
// The read that makes a closed billing period answerable, which is the entire
// reason the table exists: usage is a live measurement of bytes currently on
// disk, and nothing recovers last March's figure once April has overwritten it.
func (s *Server) handleAdminMetering(w http.ResponseWriter, r *http.Request) {
	if s.billingUnavailable(w, r) {
		return
	}
	q := r.URL.Query()

	filter := billing.RecordFilter{
		Limit:  atoiDefault(q.Get("limit"), 0),
		Offset: atoiDefault(q.Get("offset"), 0),
	}
	if raw := q.Get("owner_id"); raw != "" {
		owner, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid owner id")
			return
		}
		filter.OwnerID = &owner
	}
	from, ok := s.timeParam(w, r, q.Get("from"), "from")
	if !ok {
		return
	}
	to, ok := s.timeParam(w, r, q.Get("to"), "to")
	if !ok {
		return
	}
	filter.From, filter.To = from, to

	records, err := s.billingStore().ListRecords(r.Context(), filter)
	if err != nil {
		s.serverError(w, r, "list metering records", err)
		return
	}

	out := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		entry := map[string]any{
			"id":           rec.ID,
			"owner_id":     rec.OwnerID,
			"owner":        rec.Owner,
			"period_start": rec.Start.UTC().Format(time.RFC3339),
			"period_end":   rec.End.UTC().Format(time.RFC3339),
			// The parts and the total, for the reason the admin user list reports
			// them separately: a full account has to be explicable as "empty the
			// trash", "wait for retention" or "buy a disk", and a lone total
			// explains none of the three.
			"live_bytes":       rec.LiveBytes,
			"trash_bytes":      rec.TrashBytes,
			"version_bytes":    rec.VersionBytes,
			"total_bytes":      rec.TotalBytes(),
			"peak_total_bytes": rec.PeakTotalBytes,
			"file_count":       rec.FileCount,
			// samples is what distinguishes "this account stored nothing" from
			// "nobody was measuring", which is the first question anybody asks
			// when a period looks wrong months later.
			"samples":     rec.Samples,
			"plan":        rec.PlanName,
			"recorded_at": rec.RecordedAt.UTC().Format(time.RFC3339),
		}
		if rec.QuotaBytes != nil {
			entry["quota_bytes"] = *rec.QuotaBytes
		}
		out = append(out, entry)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"records": out})
}

// billingAssignments is a best-effort read of every account's plan, used to
// decorate the admin user list.
//
// Best effort on purpose: a billing table that is missing or slow must not take
// down the user list, which is the console's primary function and predates this
// slice by two phases.
func (s *Server) billingAssignments(ctx context.Context) map[uuid.UUID]*billing.Assignment {
	store := s.billingStore()
	if store == nil {
		return nil
	}
	assignments, err := store.Assignments(ctx)
	if err != nil {
		s.log.Warn("could not read billing plan assignments", "error", err)
		return nil
	}
	return assignments
}
