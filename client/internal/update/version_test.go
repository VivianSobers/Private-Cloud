package update

import "testing"

func TestParseVersion(t *testing.T) {
	ok := map[string]Version{
		"1.2.3":       {Major: 1, Minor: 2, Patch: 3},
		"v1.2.3":      {Major: 1, Minor: 2, Patch: 3},
		" v0.0.1 ":    {Patch: 1},
		"v1.2.3-rc.1": {Major: 1, Minor: 2, Patch: 3, Pre: "rc.1"},
		"v1.2.3+meta": {Major: 1, Minor: 2, Patch: 3},
		"v10.20.30":   {Major: 10, Minor: 20, Patch: 30},
	}
	for in, want := range ok {
		got, valid := ParseVersion(in)
		if !valid {
			t.Errorf("ParseVersion(%q): not parsed", in)
			continue
		}
		if got != want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", in, got, want)
		}
	}

	// Everything here is something the updater must refuse to order rather than
	// guess at. A git-describe string is the interesting one: it means "after
	// 1.2.3, by four commits", which is not a point on the version line.
	bad := []string{
		"", "dev", "v1.2", "1.2.3.4", "v1.2.x", "v1.02.3",
		"v1.2.3-4-gabc1234", "v1.2.3-4-gabc1234-dirty", "-1.2.3",
	}
	for _, in := range bad {
		if v, valid := ParseVersion(in); valid {
			t.Errorf("ParseVersion(%q) = %+v, want refusal", in, v)
		}
	}
}

func TestCompareOrdersReleases(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.2.4", "1.2.3", 1},
		{"1.3.0", "2.0.0", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.2.0-rc.1", "1.2.0", -1}, // a pre-release precedes its own release
		{"1.2.0", "1.2.0-rc.1", 1},
		{"1.2.0-rc.1", "1.2.0-rc.2", -1},
		{"1.2.0-beta.1", "1.2.0-rc.1", -1},
	}
	for _, c := range cases {
		a, okA := ParseVersion(c.a)
		b, okB := ParseVersion(c.b)
		if !okA || !okB {
			t.Fatalf("fixture does not parse: %q %q", c.a, c.b)
		}
		if got := Compare(a, b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		current, candidate    string
		newer, comparableWant bool
	}{
		{"v1.2.3", "v1.2.4", true, true},
		{"v1.2.4", "v1.2.3", false, true},
		{"v1.2.3", "v1.2.3", false, true},
		// A dev build is never auto-updated: replacing a binary somebody built
		// themselves is a surprise, not an update.
		{"dev", "v1.2.3", false, false},
		{"v1.2.3-4-gabc1234", "v1.2.4", false, false},
		{"v1.2.3", "not-a-version", false, false},
	}
	for _, c := range cases {
		newer, comparable := Newer(c.current, c.candidate)
		if newer != c.newer || comparable != c.comparableWant {
			t.Errorf("Newer(%q, %q) = (%v, %v), want (%v, %v)",
				c.current, c.candidate, newer, comparable, c.newer, c.comparableWant)
		}
	}
}
