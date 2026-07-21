package files

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{
		"report.pdf",
		"Photos",
		"a",
		"file with spaces.txt",
		"ünïcödé.txt",
		"日本語.md",
		"dot.in.the.middle.tar.gz",
		".hidden",
		"..hidden", // leading dots are fine; only exactly ".." is not
		"100% done",
		"under_score",
	}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := map[string]string{
		"":             "empty",
		".":            "current directory",
		"..":           "parent directory",
		"a/b":          "forward slash",
		`a\b`:          "backslash",
		"trailing ":    "trailing space",
		"trailing.":    "trailing dot",
		"co:lon":       "colon (breaks macOS and Windows)",
		"quo\"te":      "double quote",
		"pipe|d":       "pipe",
		"quest?ion":    "question mark",
		"aste*risk":    "asterisk",
		"less<than":    "angle bracket",
		"null\x00byte": "NUL byte",
		"new\nline":    "control character",
		"CON":          "Windows reserved device",
		"con.txt":      "Windows reserved device with extension",
		"COM1":         "Windows reserved port",
		"lpt9.log":     "Windows reserved printer port",
		"NUL":          "Windows reserved device",
	}
	for name, why := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error (%s)", name, why)
		}
	}

	// 255 bytes is the practical filename limit on ext4 and ZFS. Accepting more
	// here would mean an export in a later phase fails on names this server
	// already promised were fine.
	if err := ValidateName(strings.Repeat("a", 256)); err == nil {
		t.Error("ValidateName accepted a 256-byte name, want error")
	}
	if err := ValidateName(strings.Repeat("a", 255)); err != nil {
		t.Errorf("ValidateName rejected a 255-byte name: %v", err)
	}
}

func TestWindowsReservedDoesNotOverreach(t *testing.T) {
	// These merely start with a reserved prefix; refusing them would be wrong,
	// and "console.log" is common enough that getting this wrong would be
	// noticed immediately.
	ok := []string{"console.log", "COM10", "CONTACTS", "nullable.go", "auxiliary"}
	for _, name := range ok {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
}

func TestJoinPath(t *testing.T) {
	cases := []struct {
		parent, name, want string
	}{
		{"/", "photos", "/photos"},
		{"/photos", "2026", "/photos/2026"},
		{"/photos/2026", "img.jpg", "/photos/2026/img.jpg"},
	}
	for _, c := range cases {
		if got := JoinPath(c.parent, c.name); got != c.want {
			t.Errorf("JoinPath(%q, %q) = %q, want %q", c.parent, c.name, got, c.want)
		}
	}
}

func TestFold(t *testing.T) {
	if Fold("Photos") != Fold("photos") {
		t.Error("Fold must make Photos and photos collide — that collision is the point")
	}
	if Fold("a") == Fold("b") {
		t.Error("Fold must not collapse distinct names")
	}
}

func TestLikePrefixEscapesWildcards(t *testing.T) {
	// A folder genuinely named "100%_done" must not turn a subtree update into
	// a wildcard that rewrites unrelated paths.
	got := likePrefix("/100%_done")
	want := `/100\%\_done`
	if got != want {
		t.Errorf("likePrefix = %q, want %q", got, want)
	}
	if likePrefix(`/back\slash`) != `/back\\slash` {
		t.Errorf("likePrefix must escape backslashes, got %q", likePrefix(`/back\slash`))
	}
}

func TestDetectMIME(t *testing.T) {
	cases := []struct {
		name, declared, wantPrefix string
	}{
		// The extension wins: clients send octet-stream for anything they do
		// not recognise, and trusting that makes every download an attachment.
		{"photo.png", "application/octet-stream", "image/png"},
		{"notes.txt", "application/octet-stream", "text/plain"},
		// Case-insensitive: Windows clients routinely send upper-case
		// extensions from digital cameras.
		{"IMG_0001.JPG", "", "image/jpeg"},
		{"noext", "text/plain", "text/plain"},
		{"noext", "", "application/octet-stream"},
		{"noext", "not a media type at all", "application/octet-stream"},
		{"archive.unknownext", "", "application/octet-stream"},
	}
	for _, c := range cases {
		got := DetectMIME(c.name, c.declared)
		if len(got) < len(c.wantPrefix) || got[:len(c.wantPrefix)] != c.wantPrefix {
			t.Errorf("DetectMIME(%q, %q) = %q, want prefix %q", c.name, c.declared, got, c.wantPrefix)
		}
	}
}
