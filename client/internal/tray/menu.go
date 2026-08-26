package tray

import (
	"context"
	"fmt"
	"sync"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/control"
)

// Controller is the slice of the control client the menu drives. It is an
// interface for the same reason control.Engine is one: the menu's whole job is
// deciding what to show and what to send, and that decision is only testable if
// the thing it sends to can be a stub with no daemon behind it.
// *control.Client satisfies it.
type Controller interface {
	Status(ctx context.Context) (control.StatusResponse, error)
	Sync(ctx context.Context) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	ClearConflicts(ctx context.Context) error
}

// ItemID names one menu entry. The platform shell builds its real menu items
// once, in Layout order, and thereafter only re-labels, enables and hides them —
// so an id, not a position, is what the shell holds on to.
type ItemID int

const (
	ItemStatus           ItemID = iota // the summary line; never clickable
	ItemSyncNow                        // reconcile now
	ItemPauseResume                    // pause or resume automatic syncing
	ItemConflicts                      // "N conflicts…" — opens the folder holding the copies
	ItemDismissConflicts               // clear the daemon's conflict log
	ItemOpenFolder                     // open the synced folder in the file manager
	ItemOpenWeb                        // open the web app
	ItemQuit                           // close the tray, leaving the daemon running
)

// Entry is one row of the fixed menu skeleton. SeparatorBefore asks the shell for
// a divider above the item; the skeleton is fixed because a menu that adds and
// removes rows as state changes is a menu whose entries move under the pointer.
type Entry struct {
	ID              ItemID
	SeparatorBefore bool
}

// Layout is the menu skeleton, top to bottom. It never varies: what varies is
// each item's label, whether it is enabled, and whether it is shown.
func Layout() []Entry {
	return []Entry{
		{ID: ItemStatus},
		{ID: ItemSyncNow, SeparatorBefore: true},
		{ID: ItemPauseResume},
		{ID: ItemConflicts, SeparatorBefore: true},
		{ID: ItemDismissConflicts},
		{ID: ItemOpenFolder, SeparatorBefore: true},
		{ID: ItemOpenWeb},
		{ID: ItemQuit, SeparatorBefore: true},
	}
}

// Item is how one entry should currently render.
type Item struct {
	Label   string
	Tooltip string
	Enabled bool
	Visible bool
}

// View is everything the shell needs to draw one frame: which icon, what
// tooltip, and the state of every item in the layout.
type View struct {
	State   State
	Tooltip string
	Items   map[ItemID]Item
}

// ActionKind says what the shell must do after an item was activated. The model
// performs everything that is a control-socket call itself; what it cannot do is
// anything that needs the platform — opening a URL or a folder, and ending the
// platform's event loop — so it returns that as an instruction instead of
// growing an os/exec dependency and becoming platform code.
type ActionKind int

const (
	ActionNone ActionKind = iota // handled entirely by the model
	ActionOpen                   // hand Target to the OS: a URL or a directory
	ActionQuit                   // leave the tray (the daemon keeps running)
)

// Action is the shell's half of an activation.
type Action struct {
	Kind   ActionKind
	Target string
}

// Model is the platform-free tray: it holds the last status seen, turns it into
// a View, and turns an activated item into control-socket calls. Every decision
// about what the menu says and what an item does is made here, so the shell that
// draws it has nothing left to decide and nothing left to get wrong.
//
// It is safe for concurrent use: the shell's event loop reads a View while the
// refresh ticker writes a new status.
type Model struct {
	ctl Controller

	mu        sync.Mutex
	st        control.StatusResponse
	reachable bool
}

// NewModel returns a model that starts out offline, which is the truth until the
// first Refresh succeeds.
func NewModel(ctl Controller) *Model {
	return &Model{ctl: ctl}
}

// Refresh polls the daemon once and returns the state that results. A failed
// poll is not an error to report: an unreachable daemon is a state the tray
// exists to show, so it is recorded and rendered as "offline".
func (m *Model) Refresh(ctx context.Context) State {
	st, err := m.ctl.Status(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reachable = err == nil
	if err == nil {
		m.st = st
	}
	return Derive(m.st, m.reachable)
}

// snapshot returns the last status seen and whether it is live.
func (m *Model) snapshot() (control.StatusResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st, m.reachable
}

// View renders the current menu.
func (m *Model) View() View {
	st, reachable := m.snapshot()
	state := Derive(st, reachable)
	conflicts := len(st.Conflicts)

	pauseLabel := "Pause syncing"
	if st.Paused {
		pauseLabel = "Resume syncing"
	}

	conflictLabel := "No conflicts"
	if conflicts > 0 {
		conflictLabel = fmt.Sprintf("%s — show the copies", conflictNoun(conflicts))
	}

	return View{
		State:   state,
		Tooltip: Summary(st, reachable),
		Items: map[ItemID]Item{
			// The status line is deliberately dead: a menu item that looks clickable
			// and does nothing is worse than one that plainly cannot be clicked.
			ItemStatus: {Label: Summary(st, reachable), Enabled: false, Visible: true},

			ItemSyncNow: {
				Label: "Sync now", Visible: true,
				// Sync now stays available while paused — pausing stops the automatic
				// cadences only, so "paused" must never mean "stuck".
				Enabled: reachable,
				Tooltip: "Reconcile with the server immediately",
			},
			ItemPauseResume: {Label: pauseLabel, Enabled: reachable, Visible: true,
				Tooltip: "Stop or restart automatic syncing on this device"},

			// The conflict rows appear only when there is something to act on, so an
			// account that never conflicts never sees the vocabulary.
			ItemConflicts: {Label: conflictLabel, Enabled: conflicts > 0 && st.Root != "",
				Visible: conflicts > 0, Tooltip: "Open the folder holding the conflict copies"},
			ItemDismissConflicts: {Label: "Dismiss conflict warnings", Enabled: reachable,
				Visible: conflicts > 0, Tooltip: "Clear the list once you have kept the version you want"},

			ItemOpenFolder: {Label: "Open sync folder", Enabled: st.Root != "", Visible: true},
			ItemOpenWeb:    {Label: "Open web app", Enabled: st.Server != "", Visible: true},

			ItemQuit: {Label: "Quit tray", Enabled: true, Visible: true,
				Tooltip: "Close the tray icon. Syncing continues in the background"},
		},
	}
}

// conflictNoun pluralizes a conflict count.
func conflictNoun(n int) string {
	if n == 1 {
		return "1 conflict"
	}
	return fmt.Sprintf("%d conflicts", n)
}

// Activate performs what an item does. Anything reachable over the control
// socket happens here and the returned Action is ActionNone; the two things that
// need the platform come back as instructions for the shell.
//
// After a successful call the model refreshes, so the menu reflects the result
// of a click without waiting for the next tick.
func (m *Model) Activate(ctx context.Context, id ItemID) (Action, error) {
	st, _ := m.snapshot()

	switch id {
	case ItemSyncNow:
		return Action{}, m.act(ctx, m.ctl.Sync)
	case ItemPauseResume:
		if st.Paused {
			return Action{}, m.act(ctx, m.ctl.Resume)
		}
		return Action{}, m.act(ctx, m.ctl.Pause)
	case ItemDismissConflicts:
		return Action{}, m.act(ctx, m.ctl.ClearConflicts)
	case ItemConflicts, ItemOpenFolder:
		if st.Root == "" {
			return Action{}, fmt.Errorf("the daemon has not reported a sync folder yet")
		}
		return Action{Kind: ActionOpen, Target: st.Root}, nil
	case ItemOpenWeb:
		if st.Server == "" {
			return Action{}, fmt.Errorf("the daemon has not reported a server URL yet")
		}
		return Action{Kind: ActionOpen, Target: st.Server}, nil
	case ItemQuit:
		// Quitting the tray is quitting a *viewer*. The daemon is a separate
		// process with its own lifetime, and a user closing an icon is not asking
		// for their files to stop syncing.
		return Action{Kind: ActionQuit}, nil
	}
	return Action{}, nil
}

// act runs one control call and, on success, refreshes so the click's effect is
// visible immediately.
func (m *Model) act(ctx context.Context, fn func(context.Context) error) error {
	if err := fn(ctx); err != nil {
		return err
	}
	m.Refresh(ctx)
	return nil
}
