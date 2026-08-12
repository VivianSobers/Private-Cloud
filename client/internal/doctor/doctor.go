// Package doctor is pcsync's preflight: it checks, in order, the things that have
// to be true for a sync to work — a writable state database, a reachable server,
// and a credential the server accepts — and reports each as a plain pass/fail so
// a misconfiguration is diagnosed in one command instead of read from a stack
// trace. It runs standalone, without the daemon, so it works before the first
// sync and when the daemon won't start.
package doctor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/config"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/state"
)

// Status is the outcome of one check.
type Status int

const (
	Pass Status = iota
	Warn
	Fail
)

func (s Status) Glyph() string {
	switch s {
	case Pass:
		return "✓"
	case Warn:
		return "!"
	default:
		return "✗"
	}
}

// Check is one diagnostic and its result.
type Check struct {
	Name   string
	Status Status
	Detail string
}

// Result is the full preflight.
type Result struct {
	Checks []Check
}

// OK reports whether nothing failed (warnings are allowed).
func (r Result) OK() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return false
		}
	}
	return true
}

// Run performs the checks against a loaded config. The context bounds the network
// checks; the caller sets a sensible timeout.
func Run(ctx context.Context, cfg *config.Config, userAgent string) Result {
	return Result{Checks: []Check{
		configCheck(cfg),
		stateDBCheck(cfg),
		reachableCheck(ctx, cfg),
		signInCheck(ctx, cfg, userAgent),
	}}
}

// configCheck restates the loaded configuration — it has already been validated
// by config.Load, so this is context for the checks below, always a pass.
func configCheck(cfg *config.Config) Check {
	return Check{
		Name:   "configuration",
		Status: Pass,
		Detail: fmt.Sprintf("server %s · user %s · root %s", cfg.ServerURL, cfg.Username, cfg.Root),
	}
}

// stateDBCheck makes sure the local sync database can be created and opened —
// the daemon cannot record what it has synced otherwise.
func stateDBCheck(cfg *config.Config) Check {
	if err := os.MkdirAll(cfg.StateDir(), 0o700); err != nil {
		return Check{"state database", Fail, "cannot create state dir: " + err.Error()}
	}
	st, err := state.Open(cfg.StateDB)
	if err != nil {
		return Check{"state database", Fail, "cannot open: " + err.Error()}
	}
	_ = st.Close()
	return Check{"state database", Pass, cfg.StateDB}
}

// reachableCheck confirms the server answers at all, separating "server down or
// wrong URL" from "credential rejected" — an unauthenticated endpoint, so any
// HTTP response (even a 401) proves reachability.
func reachableCheck(ctx context.Context, cfg *config.Config) Check {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ServerURL+"/api/v1/auth/status", nil)
	if err != nil {
		return Check{"server reachable", Fail, err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Check{"server reachable", Fail, "cannot reach " + cfg.ServerURL + ": " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return Check{"server reachable", Warn, fmt.Sprintf("%s responded HTTP %d", cfg.ServerURL, resp.StatusCode)}
	}
	return Check{"server reachable", Pass, fmt.Sprintf("%s (HTTP %d)", cfg.ServerURL, resp.StatusCode)}
}

// signInCheck exchanges the app password for a device token and fetches the tree
// root — the end-to-end proof that the credential is accepted and this account
// can sync. A 401 is called out specifically, because it is the common mistake
// (wrong username or a mistyped app password) and its fix is different from a
// network problem.
func signInCheck(ctx context.Context, cfg *config.Config, userAgent string) Check {
	client := api.New(cfg.ServerURL, cfg.Username, cfg.AppPassword, userAgent)
	if _, err := client.GetRoot(ctx); err != nil {
		if api.IsUnauthorized(err) {
			return Check{"sign in", Fail, "credential rejected (401) — check username and app_password"}
		}
		return Check{"sign in", Fail, err.Error()}
	}
	return Check{"sign in", Pass, "credential accepted; tree root reachable"}
}

// Deadline is the default overall timeout for a run's network checks.
const Deadline = 10 * time.Second
