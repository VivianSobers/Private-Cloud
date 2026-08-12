package ignore

import "testing"

func TestMatch(t *testing.T) {
	m := Compile([]string{
		"# a comment",
		"",
		"*.tmp",         // basename glob at any depth
		".DS_Store",     // basename literal at any depth
		"node_modules/", // directory only, at any depth
		"/build",        // anchored to root
		"src/*.log",     // anchored path glob
	})

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"/scratch.tmp", false, true},             // *.tmp at root
		{"/deep/nested/scratch.tmp", false, true}, // *.tmp at depth
		{"/notes.txt", false, false},              // not matched
		{"/a/.DS_Store", false, true},             // literal basename at depth
		{"/node_modules", true, true},             // dir-only matches the dir
		{"/node_modules", false, false},           // ...but not a file of that name
		{"/pkg/node_modules", true, true},         // dir-only at depth
		{"/build", true, true},                    // anchored to root
		{"/build", false, true},                   // anchored matches file too (no trailing slash)
		{"/sub/build", false, false},              // anchored does NOT match nested
		{"/src/app.log", false, true},             // anchored path glob
		{"/src/deep/app.log", false, false},       // path.Match is single-level
		{"/", true, false},                        // never the root
	}
	for _, c := range cases {
		if got := m.Match(c.path, c.isDir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestEmptyAndComments(t *testing.T) {
	if !Compile(nil).Empty() {
		t.Error("nil lines should compile to an empty matcher")
	}
	m := Compile([]string{"# only comments", "   ", "/"})
	if !m.Empty() {
		t.Errorf("comment/blank/root-only lines should yield no rules, got %d", m.Count())
	}
	// An empty matcher matches nothing.
	if m.Match("/anything.tmp", false) {
		t.Error("empty matcher should match nothing")
	}
	var nilM *Matcher
	if !nilM.Empty() || nilM.Match("/x", false) {
		t.Error("nil matcher must be safe and match nothing")
	}
}

func TestCount(t *testing.T) {
	if n := Compile([]string{"*.tmp", "# c", "", "build/"}).Count(); n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}
