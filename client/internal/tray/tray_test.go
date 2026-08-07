package tray

import (
	"strings"
	"testing"
	"time"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/control"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/engine"
)

// Derive follows a strict precedence: unreachable dominates everything, then
// error, paused, syncing, and finally idle.
func TestDerivePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		st        control.StatusResponse
		reachable bool
		want      State
	}{
		{"unreachable beats a healthy snapshot", resp("idle", false), false, StateOffline},
		{"error beats paused", respPaused("error"), true, StateError},
		{"paused beats syncing", respPaused("syncing"), true, StatePaused},
		{"syncing when running", resp("syncing", false), true, StateSyncing},
		{"idle otherwise", resp("idle", false), true, StateIdle},
	}
	for _, c := range cases {
		if got := Derive(c.st, c.reachable); got != c.want {
			t.Errorf("%s: Derive = %v, want %v", c.name, got, c.want)
		}
	}
}

// Each state has a distinct glyph and label, so the monitor never renders two
// states identically.
func TestStateRenderingsAreDistinct(t *testing.T) {
	all := []State{StateOffline, StateError, StatePaused, StateSyncing, StateIdle}
	seenGlyph := map[string]bool{}
	seenLabel := map[string]bool{}
	for _, s := range all {
		if seenGlyph[s.Glyph()] {
			t.Errorf("duplicate glyph for %v", s)
		}
		if seenLabel[s.String()] {
			t.Errorf("duplicate label for %v", s)
		}
		seenGlyph[s.Glyph()] = true
		seenLabel[s.String()] = true
	}
}

// The summary reflects the derived state and surfaces the useful detail for each.
func TestSummary(t *testing.T) {
	if got := Summary(control.StatusResponse{}, false); got != "pcsync daemon not running" {
		t.Errorf("offline summary = %q", got)
	}

	errResp := control.StatusResponse{Status: engine.Status{Phase: "error", LastError: "connection refused"}}
	if got := Summary(errResp, true); !strings.Contains(got, "connection refused") {
		t.Errorf("error summary should carry the message: %q", got)
	}

	idle := control.StatusResponse{Status: engine.Status{
		Phase:     "idle",
		Tracked:   1,
		LastSync:  time.Now().Add(-30 * time.Second),
		Conflicts: []engine.ConflictRecord{{Original: "/a"}},
	}}
	got := Summary(idle, true)
	if !strings.Contains(got, "1 item") {
		t.Errorf("singular pluralization wrong: %q", got)
	}
	if !strings.Contains(got, "last sync") || !strings.Contains(got, "conflict") {
		t.Errorf("idle summary missing detail: %q", got)
	}
}

// RelTime is coarse and bucketed, and never shows a negative age for a clock that
// is momentarily ahead.
func TestRelTime(t *testing.T) {
	if got := RelTime(time.Time{}); got != "never" {
		t.Errorf("zero time = %q, want never", got)
	}
	if got := RelTime(time.Now().Add(-45 * time.Second)); !strings.HasSuffix(got, "s ago") {
		t.Errorf("seconds bucket = %q", got)
	}
	if got := RelTime(time.Now().Add(-5 * time.Minute)); !strings.HasSuffix(got, "m ago") {
		t.Errorf("minutes bucket = %q", got)
	}
	if got := RelTime(time.Now().Add(time.Hour)); got != "0s ago" {
		t.Errorf("future time = %q, want clamped to 0s ago", got)
	}
}

func resp(phase string, paused bool) control.StatusResponse {
	return control.StatusResponse{Status: engine.Status{Phase: engine.Phase(phase), Paused: paused}}
}

func respPaused(phase string) control.StatusResponse {
	return control.StatusResponse{Status: engine.Status{Phase: engine.Phase(phase), Paused: true}}
}
