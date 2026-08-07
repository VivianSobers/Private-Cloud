// Package tray turns the daemon's raw status into what a person sees: a single
// tray state (the icon to show), a one-line summary (the tooltip or status line),
// and the small helpers a menu needs. It holds no I/O and no platform code, so it
// is shared, unchanged, by the headless `pcsync watch` monitor and by the desktop
// tray shell — the shell is a thin adapter that maps these states to a real icon
// and menu, and everything it decides is decided and tested here.
package tray

import (
	"fmt"
	"time"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/control"
)

// State is the coarse condition a tray icon reflects, in precedence order: a
// daemon we cannot reach outranks any stale status it might have reported.
type State int

const (
	StateOffline State = iota // the control socket could not be reached
	StateError                // the last reconcile failed
	StatePaused               // automatic syncing is paused
	StateSyncing              // a reconcile is in flight
	StateIdle                 // up to date, waiting
)

// String is a stable lowercase label, handy for logs and tests.
func (s State) String() string {
	switch s {
	case StateOffline:
		return "offline"
	case StateError:
		return "error"
	case StatePaused:
		return "paused"
	case StateSyncing:
		return "syncing"
	default:
		return "idle"
	}
}

// Glyph is a single-rune stand-in for the state, used by the text monitor and as
// a fallback where a real icon is unavailable.
func (s State) Glyph() string {
	switch s {
	case StateOffline:
		return "○"
	case StateError:
		return "✗"
	case StatePaused:
		return "⏸"
	case StateSyncing:
		return "↻"
	default:
		return "✓"
	}
}

// Derive maps a status — or the failure to fetch one — to a tray state.
// reachable is false when the daemon's control socket could not be reached, which
// dominates: an unreachable daemon is "offline" regardless of any prior snapshot.
func Derive(st control.StatusResponse, reachable bool) State {
	switch {
	case !reachable:
		return StateOffline
	case st.Phase == "error":
		return StateError
	case st.Paused:
		return StatePaused
	case st.Phase == "syncing":
		return StateSyncing
	default:
		return StateIdle
	}
}

// Summary is a one-line description for a tooltip or a live status line.
func Summary(st control.StatusResponse, reachable bool) string {
	switch Derive(st, reachable) {
	case StateOffline:
		return "pcsync daemon not running"
	case StateError:
		return "Sync error: " + st.LastError
	case StatePaused:
		return fmt.Sprintf("Paused — %s", items(st.Tracked))
	case StateSyncing:
		return fmt.Sprintf("Syncing… — %s", items(st.Tracked))
	default:
		line := fmt.Sprintf("Up to date — %s", items(st.Tracked))
		if !st.LastSync.IsZero() {
			line += " · last sync " + RelTime(st.LastSync)
		}
		if n := len(st.Conflicts); n > 0 {
			line += fmt.Sprintf(" · %d conflict(s)", n)
		}
		return line
	}
}

// items renders a tracked-item count with correct pluralization.
func items(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}

// HumanBytes renders a byte count in binary units (KiB/MiB/…) to one decimal,
// for the transfer stats a status line shows.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// RelTime renders a timestamp as a coarse "how long ago", or "never" for zero. It
// is the one place relative time is formatted, shared by the monitor and the CLI.
func RelTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02 15:04")
	}
}
