package state

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutGetDelete(t *testing.T) {
	s := openTemp(t)

	if _, ok, err := s.Get("/a.txt"); err != nil || ok {
		t.Fatalf("unexpected entry before put: ok=%v err=%v", ok, err)
	}

	e := Entry{Path: "/a.txt", NodeID: "n1", Kind: "file", Size: 12, MtimeUnix: 1000, Hash: "abc"}
	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get("/a.txt")
	if err != nil || !ok {
		t.Fatalf("get after put: ok=%v err=%v", ok, err)
	}
	if got != e {
		t.Errorf("round-trip mismatch: %+v != %+v", got, e)
	}

	// Put again replaces in place.
	e.Hash = "def"
	e.Size = 20
	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Get("/a.txt")
	if got.Hash != "def" || got.Size != 20 {
		t.Errorf("upsert did not replace: %+v", got)
	}

	if err := s.Delete("/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("/a.txt"); ok {
		t.Error("entry survived delete")
	}
	// Deleting an absent path is a no-op, not an error.
	if err := s.Delete("/gone.txt"); err != nil {
		t.Errorf("delete of absent path errored: %v", err)
	}
}

func TestListOrderedAndEmpty(t *testing.T) {
	s := openTemp(t)

	if empty, err := s.Empty(); err != nil || !empty {
		t.Fatalf("fresh store should be empty: empty=%v err=%v", empty, err)
	}

	for _, p := range []string{"/b", "/a", "/a/child"} {
		if err := s.Put(Entry{Path: p, NodeID: p, Kind: "folder"}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Path != "/a" || list[1].Path != "/a/child" || list[2].Path != "/b" {
		t.Errorf("list not path-ordered: %+v", list)
	}
	if empty, _ := s.Empty(); empty {
		t.Error("store reports empty after puts")
	}
}

func TestCursorPersists(t *testing.T) {
	s := openTemp(t)

	if c, err := s.Cursor(); err != nil || c != 0 {
		t.Fatalf("fresh cursor should be 0: c=%d err=%v", c, err)
	}
	if err := s.SetCursor(42); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Cursor(); c != 42 {
		t.Errorf("cursor = %d, want 42", c)
	}
	if err := s.SetCursor(100); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Cursor(); c != 100 {
		t.Errorf("cursor = %d, want 100", c)
	}
}
