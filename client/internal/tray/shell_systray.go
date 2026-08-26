//go:build tray

// This file is the entire platform half of the tray, and the only file in the
// client that imports a GUI library. It is compiled only with `-tags tray`.

package tray

import (
	"context"
	"time"

	"fyne.io/systray"
)

// Supported reports whether this build can draw a real tray.
const Supported = true

// systrayShell renders the model with fyne.io/systray — a maintained fork of
// getlantern/systray, chosen because it is the one library covering Windows,
// macOS and Linux whose Windows and Linux backends are pure Go (Win32 syscalls
// and a StatusNotifierItem over D-Bus), so only a macOS build needs cgo at all.
type systrayShell struct{}

// NewShell returns the shell this build supports.
func NewShell() Shell { return systrayShell{} }

// maxTooltip is the shortest tooltip limit across the three platforms (Windows
// truncates at 128 UTF-16 units). Truncating here means every platform shows the
// same string rather than one of them silently showing a different one.
const maxTooltip = 120

// Run builds the menu once and then drives it until the user picks Quit or ctx
// is cancelled. It blocks: systray owns the calling thread's run loop, which is
// why the caller must be main.
func (systrayShell) Run(ctx context.Context, m *Model, opts Options) error {
	opts = opts.withDefaults()

	// A first poll before the icon appears, so the tray never flashes "offline"
	// on the way to the truth.
	m.Refresh(ctx)

	systray.Run(func() { onReady(ctx, m, opts) }, func() {})
	return nil
}

// onReady creates the menu items in Layout order and starts the loop that keeps
// them in step with the model.
func onReady(ctx context.Context, m *Model, opts Options) {
	items := make(map[ItemID]*systray.MenuItem, len(Layout()))
	clicks := make(chan ItemID, 1)

	for _, e := range Layout() {
		if e.SeparatorBefore {
			systray.AddSeparator()
		}
		mi := systray.AddMenuItem("", "")
		items[e.ID] = mi

		// systray gives every item its own channel, so one forwarding goroutine per
		// item collapses them into a single select below. They live as long as the
		// tray does, which is as long as the process does.
		id := e.ID
		go func() {
			for range mi.ClickedCh {
				select {
				case clicks <- id:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	apply(m.View(), items)

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			systray.Quit()
			return
		case <-ticker.C:
			m.Refresh(ctx)
			apply(m.View(), items)
		case id := <-clicks:
			act, err := m.Activate(ctx, id)
			if err != nil {
				// A click that failed is worth a log line and nothing more: the daemon
				// is the authority on what happened, and the next poll — a second away
				// — will say so in the status line the user is already looking at.
				opts.Log.Error("tray action failed", "item", id, "error", err)
			}
			switch act.Kind {
			case ActionOpen:
				if err := opts.Open(act.Target); err != nil {
					opts.Log.Error("tray could not open target", "target", act.Target, "error", err)
				}
			case ActionQuit:
				systray.Quit()
				return
			}
			apply(m.View(), items)
		}
	}
}

// apply pushes one View onto the real menu. Every item already exists, so this
// only ever re-labels, enables, shows and hides — the menu's shape is fixed, and
// nothing under the pointer moves between two frames.
func apply(v View, items map[ItemID]*systray.MenuItem) {
	systray.SetIcon(Icon(v.State))
	systray.SetTooltip(truncate(v.Tooltip, maxTooltip))
	// SetTitle is a no-op on Windows and shows text beside the icon elsewhere; the
	// glyph is the compact form of the same state the icon carries.
	systray.SetTitle(v.State.Glyph())

	for id, mi := range items {
		it, ok := v.Items[id]
		if !ok {
			mi.Hide()
			continue
		}
		mi.SetTitle(it.Label)
		mi.SetTooltip(it.Tooltip)
		if it.Enabled {
			mi.Enable()
		} else {
			mi.Disable()
		}
		if it.Visible {
			mi.Show()
		} else {
			mi.Hide()
		}
	}
}

// truncate shortens a string to at most n runes, marking that it was cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
