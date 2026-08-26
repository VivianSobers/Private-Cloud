package tray

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/control"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/engine"
)

// fakeCtl is a control client with no daemon behind it: it answers with whatever
// status the test set and records what was sent.
type fakeCtl struct {
	st       control.StatusResponse
	err      error // returned by Status, making the daemon unreachable
	statusN  int
	sent     []string
	actionEr error
}

func (f *fakeCtl) Status(context.Context) (control.StatusResponse, error) {
	f.statusN++
	if f.err != nil {
		return control.StatusResponse{}, f.err
	}
	return f.st, nil
}

func (f *fakeCtl) call(name string) error {
	f.sent = append(f.sent, name)
	return f.actionEr
}

func (f *fakeCtl) Sync(context.Context) error           { return f.call("sync") }
func (f *fakeCtl) Pause(context.Context) error          { return f.call("pause") }
func (f *fakeCtl) Resume(context.Context) error         { return f.call("resume") }
func (f *fakeCtl) ClearConflicts(context.Context) error { return f.call("clear") }

func newFake(st control.StatusResponse) (*fakeCtl, *Model) {
	f := &fakeCtl{st: st}
	m := NewModel(f)
	m.Refresh(context.Background())
	return f, m
}

func idleStatus() control.StatusResponse {
	return control.StatusResponse{
		Status: engine.Status{Phase: "idle", Tracked: 3},
		Server: "https://cloud.example.ts.net",
		Root:   "/home/me/Cloud",
	}
}

// Every entry the layout names is rendered by the view, and the view invents no
// entry the layout does not place — the two are the same menu described twice,
// and a shell built from one and updated from the other would silently drop rows.
func TestLayoutAndViewAgree(t *testing.T) {
	_, m := newFake(idleStatus())
	v := m.View()

	for _, e := range Layout() {
		if _, ok := v.Items[e.ID]; !ok {
			t.Errorf("layout entry %v has no item in the view", e.ID)
		}
	}
	if len(v.Items) != len(Layout()) {
		t.Errorf("view has %d items, layout has %d", len(v.Items), len(Layout()))
	}
	if v.Items[ItemStatus].Enabled {
		t.Error("the status line must not be clickable")
	}
}

// With no daemon to reach, every control action is disabled and the menu says so
// rather than offering buttons that cannot work — but the tray itself can still
// be quit.
func TestViewWhenOffline(t *testing.T) {
	f := &fakeCtl{err: errors.New("no such file or directory")}
	m := NewModel(f)
	m.Refresh(context.Background())

	v := m.View()
	if v.State != StateOffline {
		t.Fatalf("state = %v, want offline", v.State)
	}
	for _, id := range []ItemID{ItemSyncNow, ItemPauseResume, ItemOpenFolder, ItemOpenWeb} {
		if v.Items[id].Enabled {
			t.Errorf("item %v should be disabled while the daemon is unreachable", id)
		}
	}
	if !v.Items[ItemQuit].Enabled {
		t.Error("quit must stay available — it closes the tray, not the daemon")
	}
}

// Sync now survives a pause, because pausing stops the automatic cadences only.
func TestSyncNowStaysAvailableWhilePaused(t *testing.T) {
	st := idleStatus()
	st.Paused = true
	f, m := newFake(st)

	v := m.View()
	if !v.Items[ItemSyncNow].Enabled {
		t.Error("sync now must stay enabled while paused")
	}
	if got := v.Items[ItemPauseResume].Label; got != "Resume syncing" {
		t.Errorf("pause item label = %q, want Resume syncing", got)
	}

	if _, err := m.Activate(context.Background(), ItemPauseResume); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "resume" {
		t.Errorf("sent %v, want one resume", f.sent)
	}
}

// The same row sends pause when running, and the model re-polls afterwards so
// the label flips without waiting for the next tick.
func TestPauseSendsPauseAndRefreshes(t *testing.T) {
	f, m := newFake(idleStatus())
	before := f.statusN

	if _, err := m.Activate(context.Background(), ItemPauseResume); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "pause" {
		t.Errorf("sent %v, want one pause", f.sent)
	}
	if f.statusN <= before {
		t.Error("a successful action should refresh the status")
	}
}

// A failed action is reported and does not refresh: the daemon did not do
// anything, so there is nothing new to show.
func TestFailedActionIsReported(t *testing.T) {
	f, m := newFake(idleStatus())
	f.actionEr = errors.New("control request: connection refused")
	before := f.statusN

	if _, err := m.Activate(context.Background(), ItemSyncNow); err == nil {
		t.Fatal("expected the control error to surface")
	}
	if f.statusN != before {
		t.Error("a failed action should not refresh")
	}
}

// The conflict rows exist only when there is a conflict to act on.
func TestConflictRowsAppearOnlyWhenThereAreConflicts(t *testing.T) {
	f, m := newFake(idleStatus())
	if v := m.View(); v.Items[ItemConflicts].Visible || v.Items[ItemDismissConflicts].Visible {
		t.Error("conflict rows should be hidden with no conflicts")
	}

	f.st.Conflicts = []engine.ConflictRecord{{Original: "/a", Copy: "/a (conflict)"}}
	m.Refresh(context.Background())

	v := m.View()
	if !v.Items[ItemConflicts].Visible || !v.Items[ItemConflicts].Enabled {
		t.Error("the conflict row should appear once there is a conflict")
	}
	if v.Items[ItemConflicts].Label != "1 conflict — show the copies" {
		t.Errorf("conflict label = %q", v.Items[ItemConflicts].Label)
	}
	if _, err := m.Activate(context.Background(), ItemDismissConflicts); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "clear" {
		t.Errorf("sent %v, want one clear", f.sent)
	}
}

// The two things the model cannot do itself come back as instructions, carrying
// the target the daemon reported rather than anything the model invented.
func TestActivateReturnsPlatformActions(t *testing.T) {
	_, m := newFake(idleStatus())
	ctx := context.Background()

	for _, c := range []struct {
		id     ItemID
		want   ActionKind
		target string
	}{
		{ItemOpenWeb, ActionOpen, "https://cloud.example.ts.net"},
		{ItemOpenFolder, ActionOpen, "/home/me/Cloud"},
		{ItemConflicts, ActionOpen, "/home/me/Cloud"},
		{ItemQuit, ActionQuit, ""},
		{ItemStatus, ActionNone, ""},
	} {
		act, err := m.Activate(ctx, c.id)
		if err != nil {
			t.Fatalf("%v: %v", c.id, err)
		}
		if act.Kind != c.want || act.Target != c.target {
			t.Errorf("%v: got %v/%q, want %v/%q", c.id, act.Kind, act.Target, c.want, c.target)
		}
	}
}

// Before the first successful status there is no folder or server to open, and
// asking for one is an error rather than an empty target handed to a shell.
func TestOpenRefusesAnUnknownTarget(t *testing.T) {
	f := &fakeCtl{err: errors.New("unreachable")}
	m := NewModel(f)
	for _, id := range []ItemID{ItemOpenFolder, ItemOpenWeb} {
		if _, err := m.Activate(context.Background(), id); err == nil {
			t.Errorf("%v: expected an error with nothing reported yet", id)
		}
	}
}

// OpenTarget passes only an http(s) URL or an absolute path to the desktop, so
// nothing else can ride the daemon's status into a shell.
func TestOpenTargetRefusesAnythingElse(t *testing.T) {
	for _, bad := range []string{"", "relative/path", "file:///etc/shadow", "ftp://host/x", "-e"} {
		if err := OpenTarget(bad); err == nil {
			t.Errorf("OpenTarget(%q) should have been refused", bad)
		}
	}
}

// Every state has its own icon, in both encodings, and the PNGs really are PNGs
// of the size the tray asks for — an icon that fails to decode is a blank square
// on somebody's taskbar with nothing in the logs.
func TestIconsAreDistinctAndDecodable(t *testing.T) {
	states := []State{StateOffline, StateError, StatePaused, StateSyncing, StateIdle}
	seen := map[string]State{}

	for _, s := range states {
		p := IconPNG(s)
		img, err := png.Decode(bytes.NewReader(p))
		if err != nil {
			t.Fatalf("%v: PNG does not decode: %v", s, err)
		}
		if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
			t.Errorf("%v: PNG is %dx%d, want 32x32", s, b.Dx(), b.Dy())
		}
		if prev, dup := seen[string(p)]; dup {
			t.Errorf("%v and %v share an icon", s, prev)
		}
		seen[string(p)] = s

		// An .ico begins with a two-byte zero, then type 1 (icon), little-endian.
		ic := IconICO(s)
		if len(ic) < 22 || !bytes.HasPrefix(ic, []byte{0, 0, 1, 0}) {
			t.Errorf("%v: .ico header is wrong", s)
		}
	}
	if len(Icon(StateIdle)) == 0 {
		t.Error("Icon returned nothing for this platform")
	}
}
