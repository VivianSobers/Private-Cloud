package httpapi

import (
	"strings"
	"testing"
)

func TestContentDisposition(t *testing.T) {
	cases := []struct {
		name          string
		forceDownload bool
		wantContains  []string
	}{
		{"report.pdf", false, []string{`inline; filename="report.pdf"`, "filename*=UTF-8''report.pdf"}},
		{"report.pdf", true, []string{`attachment; filename="report.pdf"`}},
		// Non-ASCII cannot ride in the bare filename= parameter, so it is
		// replaced there and carried properly by filename*=.
		{"日本語.txt", false, []string{`filename="___.txt"`, "filename*=UTF-8''"}},
	}
	for _, c := range cases {
		got := contentDisposition(c.name, c.forceDownload)
		for _, want := range c.wantContains {
			if !strings.Contains(got, want) {
				t.Errorf("contentDisposition(%q, %t) = %q, want it to contain %q",
					c.name, c.forceDownload, got, want)
			}
		}
	}
}

func TestContentDispositionCannotInjectParameters(t *testing.T) {
	// An unescaped quote would end the filename parameter early and let a
	// crafted name append header parameters of its own.
	got := contentDisposition(`evil"; malicious="1`, false)
	if strings.Count(got, `"`) != 2 {
		t.Errorf("contentDisposition produced unbalanced quotes: %q", got)
	}
	if strings.Contains(got, `malicious="1"`) {
		t.Errorf("filename injected a header parameter: %q", got)
	}
}

func TestContentDispositionEmptyFallback(t *testing.T) {
	// A name made entirely of non-ASCII collapses to underscores, never to an
	// empty filename= that some clients reject outright.
	got := contentDisposition("日本語", false)
	if strings.Contains(got, `filename=""`) {
		t.Errorf("empty ASCII fallback: %q", got)
	}
}

func TestLastPathSegment(t *testing.T) {
	cases := map[string]string{
		`C:\Users\me\report.pdf`: "report.pdf",
		"docs/report.pdf":        "report.pdf",
		"report.pdf":             "report.pdf",
		"  spaced.txt  ":         "spaced.txt",
		"":                       "",
	}
	for in, want := range cases {
		if got := lastPathSegment(in); got != want {
			t.Errorf("lastPathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
