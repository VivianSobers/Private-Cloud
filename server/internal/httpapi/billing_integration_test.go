package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs/billing"
)

// Billing hooks (Phase 9 slice 5).
//
// The slice is a seam rather than a product, so what these tests owe is not
// "does it charge correctly" — nothing charges anything. It is the three
// properties the seam would be worthless without:
//
//   - a plan DRIVES the quota that already exists, rather than becoming a second
//     limit beside it;
//   - a metering record holds THE SAME NUMBERS /usage reports, so a bill and a
//     storage page can never disagree;
//   - none of it is reachable by a non-admin, because a plan is what an account
//     may store and a metering record is how much everybody stored.

// createPlan adds a plan as an admin and returns its id.
func (f *apiFixture) createPlan(t *testing.T, name string, quota *int64) string {
	t.Helper()
	body := map[string]any{"name": name}
	if quota != nil {
		body["quota_bytes"] = *quota
	}
	rec := f.do(http.MethodPost, "/api/v1/admin/billing/plans", jsonBody(t, body), f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("create plan = %d: %s", rec.Code, rec.Body)
	}
	return decode(t, rec)["plan"].(map[string]any)["id"].(string)
}

// setPlan puts the fixture's main user on a plan, as an admin would.
func (f *apiFixture) setPlan(t *testing.T, planID any) map[string]any {
	t.Helper()
	rec := f.do(http.MethodPut,
		"/api/v1/admin/billing/accounts/"+f.userID.String()+"/plan",
		jsonBody(t, map[string]any{"plan_id": planID}), f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("set plan = %d: %s", rec.Code, rec.Body)
	}
	return decode(t, rec)
}

// The property the whole slice turns on. A plan is not a second gate: assigning
// one writes its quota through to users.quota_bytes, so the upload path — which
// has never heard of a plan — refuses exactly as it would have if an admin had
// typed the number in by hand.
func TestAssigningAPlanDrivesTheExistingQuota(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()

	small := int64(32)
	f.setPlan(t, f.createPlan(t, "starter", &small))

	// GET /usage is the user's own view, and it must report the plan's number
	// rather than a stale one — there is only one number for it to report.
	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	if usage["quota_bytes"] != float64(32) {
		t.Fatalf("quota_bytes after assignment = %v, want 32 — the plan did not drive the quota", usage["quota_bytes"])
	}

	if rec := f.upload(root, "small.txt", strings.Repeat("a", 16)); rec.Code != http.StatusCreated {
		t.Fatalf("upload inside the plan = %d: %s", rec.Code, rec.Body)
	}
	rec := f.upload(root, "big.txt", strings.Repeat("b", 1024))
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("upload over the plan = %d, want 507: %s", rec.Code, rec.Body)
	}
	if decode(t, rec)["error"].(map[string]any)["code"] != "quota_exceeded" {
		t.Error("the refusal does not carry the existing quota_exceeded code; the plan invented a second refusal path")
	}

	// Moving to a roomier plan moves the quota with it, and the same upload then
	// succeeds. Nothing in the upload path changed, which is the point.
	large := int64(1 << 20)
	f.setPlan(t, f.createPlan(t, "pro", &large))
	if rec := f.upload(root, "big.txt", strings.Repeat("b", 1024)); rec.Code != http.StatusCreated {
		t.Fatalf("upload after moving to a larger plan = %d: %s", rec.Code, rec.Body)
	}
}

// A plan with no quota is unlimited, spelled the way users.quota_bytes spells
// it: absent, never zero. Zero would be a plan of zero bytes, which is the
// opposite instruction.
func TestAPlanWithNoQuotaMeansUnlimited(t *testing.T) {
	f := newAPIFixture(t)

	tight := int64(8)
	f.setPlan(t, f.createPlan(t, "tight", &tight))
	f.setPlan(t, f.createPlan(t, "unlimited", nil))

	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	if _, present := usage["quota_bytes"]; present {
		t.Errorf("quota_bytes is still reported (%v) after moving to an unlimited plan", usage["quota_bytes"])
	}
	if rec := f.upload(f.root(), "big.txt", strings.Repeat("b", 4096)); rec.Code != http.StatusCreated {
		t.Fatalf("upload on an unlimited plan = %d: %s", rec.Code, rec.Body)
	}
}

// Detaching leaves the quota where the plan put it. Clearing it would hand
// unlimited storage to an account somebody was in the middle of removing from a
// paid tier, which is the opposite of what the action means.
func TestDetachingAPlanLeavesTheQuotaInForce(t *testing.T) {
	f := newAPIFixture(t)

	limit := int64(32)
	f.setPlan(t, f.createPlan(t, "starter-detach", &limit))
	out := f.setPlan(t, nil)
	if _, present := out["plan"]; present {
		t.Error("the account still reports a plan after being detached")
	}

	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	if usage["quota_bytes"] != float64(32) {
		t.Errorf("quota_bytes after detaching = %v, want the 32 the plan left in force", usage["quota_bytes"])
	}
}

// Changing a plan does NOT retighten the accounts already on it. A mistyped edit
// would otherwise squeeze every one of them at once, and the accounts affected
// would be exactly the ones nobody was looking at.
func TestEditingAPlanDoesNotReapplyItToExistingAccounts(t *testing.T) {
	f := newAPIFixture(t)

	roomy := int64(1 << 20)
	f.setPlan(t, f.createPlan(t, "editable", &roomy))

	// Same name, so this is the update path rather than a second plan.
	tiny := int64(1)
	f.createPlan(t, "editable", &tiny)

	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	if usage["quota_bytes"] != float64(1<<20) {
		t.Errorf("quota_bytes = %v after the plan was edited; editing a plan silently retightened a live account",
			usage["quota_bytes"])
	}
}

// The metering arithmetic. The record must hold exactly what GET /usage reports
// — not approximately, not a second query that happens to agree today. Two
// notions of a number disagree eventually, and a disagreement about how much
// somebody stored is one that surfaces on a bill.
func TestMeteringRecordsTheSameNumbersUsageReports(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()

	f.upload(root, "one.txt", strings.Repeat("a", 400))
	id := nodeID(t, f.upload(root, "two.txt", strings.Repeat("b", 600)))
	if rec := f.json(http.MethodDelete, "/api/v1/nodes/"+id, nil); rec.Code != http.StatusOK {
		t.Fatalf("trash = %d", rec.Code)
	}

	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))

	// A daily grain so one test run sits inside one period. The arithmetic under
	// test is the copying, not the calendar — PeriodFor has its own tests.
	meter := billing.NewMeter(billing.NewStore(f.pool), files.NewStore(f.pool),
		nil, billing.PeriodDaily, nil)
	if err := meter.Sweep(f.ctx); err != nil {
		t.Fatalf("metering sweep: %v", err)
	}

	rec := f.do(http.MethodGet,
		"/api/v1/admin/billing/metering?owner_id="+f.userID.String(), nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("read metering = %d: %s", rec.Code, rec.Body)
	}
	records := decode(t, rec)["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("got %d metering record(s) for one owner in one period, want exactly 1", len(records))
	}
	got := records[0].(map[string]any)

	for _, field := range []string{"live_bytes", "trash_bytes", "version_bytes", "total_bytes", "file_count"} {
		if got[field] != usage[field] {
			t.Errorf("metering %s = %v, /usage reports %v — the metering record has grown an accounting of its own",
				field, got[field], usage[field])
		}
	}

	// A second sweep in the same period updates the one row rather than inventing
	// a second, contradictory measurement of it. That is what makes the task safe
	// to run on any cadence and to restart mid-sweep.
	if err := meter.Sweep(f.ctx); err != nil {
		t.Fatalf("second metering sweep: %v", err)
	}
	rec = f.do(http.MethodGet,
		"/api/v1/admin/billing/metering?owner_id="+f.userID.String(), nil, f.admin)
	records = decode(t, rec)["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("a second sweep produced %d record(s) for one period, want 1", len(records))
	}
	if samples := records[0].(map[string]any)["samples"]; samples != float64(2) {
		t.Errorf("samples = %v after two sweeps, want 2 — the count is what separates "+
			"'this account stored nothing' from 'nobody was measuring'", samples)
	}
}

// The peak survives a delete. Otherwise the highest figure an account ever
// reached would be whatever it happened to be holding at the last tick of the
// month, which is the most flattering possible reading of a billing period.
func TestMeteringKeepsThePeakAcrossSweeps(t *testing.T) {
	f := newAPIFixture(t)
	root := f.root()

	id := nodeID(t, f.upload(root, "big.txt", strings.Repeat("a", 2000)))
	meter := billing.NewMeter(billing.NewStore(f.pool), files.NewStore(f.pool),
		nil, billing.PeriodDaily, nil)
	if err := meter.Sweep(f.ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	// Trash then purge, so the bytes are genuinely gone from the accounting.
	f.json(http.MethodDelete, "/api/v1/nodes/"+id, nil)
	f.json(http.MethodDelete, "/api/v1/trash/"+id, nil)
	if err := meter.Sweep(f.ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}

	rec := f.do(http.MethodGet,
		"/api/v1/admin/billing/metering?owner_id="+f.userID.String(), nil, f.admin)
	got := decode(t, rec)["records"].([]any)[0].(map[string]any)

	if got["total_bytes"].(float64) >= 2000 {
		t.Errorf("total_bytes = %v after the content was purged; the latest observation is stale", got["total_bytes"])
	}
	if got["peak_total_bytes"].(float64) < 2000 {
		t.Errorf("peak_total_bytes = %v, want at least the 2000 the account actually held", got["peak_total_bytes"])
	}
}

// The access rules. Every one of these is admin-only server-side; the console's
// nav gating is convenience, never the boundary.
func TestBillingEndpointsAreAdminOnly(t *testing.T) {
	f := newAPIFixture(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/billing/plans"},
		{http.MethodPost, "/api/v1/admin/billing/plans"},
		{http.MethodGet, "/api/v1/admin/billing/metering"},
		{http.MethodPut, "/api/v1/admin/billing/accounts/" + f.userID.String() + "/plan"},
	} {
		// Anonymous.
		if rec := f.do(tc.method, tc.path, strings.NewReader("{}"), nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s anonymously = %d, want 401", tc.method, tc.path, rec.Code)
		}
		// A signed-in NON-admin. This is the one that matters: the caller has a
		// valid session, and a plan is still none of their business.
		if rec := f.do(tc.method, tc.path, strings.NewReader("{}"), f.cookie); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a non-admin = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}

// A user cannot raise their own quota by naming a plan, which would be the
// obvious escalation if the route were merely session-authenticated.
func TestANonAdminCannotPutThemselvesOnAPlan(t *testing.T) {
	f := newAPIFixture(t)

	roomy := int64(1 << 30)
	planID := f.createPlan(t, "escalation", &roomy)

	tight := int64(8)
	f.setPlan(t, f.createPlan(t, "escalation-floor", &tight))

	rec := f.do(http.MethodPut, "/api/v1/admin/billing/accounts/"+f.userID.String()+"/plan",
		jsonBody(t, map[string]any{"plan_id": planID}), f.cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-assignment as a non-admin = %d, want 403: %s", rec.Code, rec.Body)
	}
	usage := decode(t, f.json(http.MethodGet, "/api/v1/usage", nil))
	if usage["quota_bytes"] != float64(8) {
		t.Errorf("quota_bytes = %v after a refused self-assignment, want the 8 it was", usage["quota_bytes"])
	}
}

// Naming a plan or an account that does not exist is a 404 with a stable shape,
// not a 500 and not a silent success.
func TestAssigningAnUnknownPlanIsRefused(t *testing.T) {
	f := newAPIFixture(t)

	rec := f.do(http.MethodPut, "/api/v1/admin/billing/accounts/"+f.userID.String()+"/plan",
		jsonBody(t, map[string]any{"plan_id": "00000000-0000-0000-0000-000000000001"}), f.admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("assigning an unknown plan = %d, want 404: %s", rec.Code, rec.Body)
	}

	planID := f.createPlan(t, "exists", nil)
	rec = f.do(http.MethodPut, "/api/v1/admin/billing/accounts/00000000-0000-0000-0000-000000000002/plan",
		jsonBody(t, map[string]any{"plan_id": planID}), f.admin)
	if rec.Code != http.StatusNotFound {
		t.Errorf("assigning to an unknown account = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// A plan is identified by its name to everyone who uses it, so the name is
// folded — "Free" and "free" are one plan, not two that render identically in a
// list and differ only in which accounts are on them.
func TestPlanNamesAreFolded(t *testing.T) {
	f := newAPIFixture(t)

	quota := int64(100)
	f.createPlan(t, "Folded", &quota)
	f.createPlan(t, "  FOLDED ", &quota)

	rec := f.do(http.MethodGet, "/api/v1/admin/billing/plans", nil, f.admin)
	plans := decode(t, rec)["plans"].([]any)

	seen := 0
	for _, raw := range plans {
		if raw.(map[string]any)["name"] == "folded" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("found %d plans named \"folded\", want exactly 1 — case created a duplicate", seen)
	}
}

// The admin user list reports the plan BESIDE the quota, never instead of it, so
// an admin who has since edited a quota directly can see that the two have
// parted company rather than being told a comfortable story about a limit that
// is no longer in force.
func TestTheAdminUserListReportsPlanAndQuotaSeparately(t *testing.T) {
	f := newAPIFixture(t)

	planQuota := int64(64)
	f.setPlan(t, f.createPlan(t, "drifting", &planQuota))

	// The direct override, which is a supported act.
	f.setQuota(t, 999)

	rec := f.do(http.MethodGet, "/api/v1/admin/users", nil, f.admin)
	for _, raw := range decode(t, rec)["users"].([]any) {
		u := raw.(map[string]any)
		if u["id"] != f.userID.String() {
			continue
		}
		if u["plan"] != "drifting" {
			t.Errorf("plan = %v, want \"drifting\"", u["plan"])
		}
		if u["plan_quota_bytes"] != float64(64) {
			t.Errorf("plan_quota_bytes = %v, want 64", u["plan_quota_bytes"])
		}
		if u["quota_bytes"] != float64(999) {
			t.Errorf("quota_bytes = %v, want the 999 an admin set directly — "+
				"the enforced number must be the one reported", u["quota_bytes"])
		}
		return
	}
	t.Fatal("the fixture user is not in the admin list")
}
