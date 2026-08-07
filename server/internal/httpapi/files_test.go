package httpapi

import (
	"strings"
	"testing"
)

// A filename containing a Content-Disposition parameter separator must not be
// able to end the filename* parameter early. url.PathEscape left ";" and "="
// alone, so `report;v2.pdf` produced a header a client parses as two parameters.
func TestContentDispositionEscapesParameterSeparators(t *testing.T) {
	const marker = "filename*=UTF-8''"
	for _, name := range []string{`report;v2.pdf`, `a=b,c.pdf`, "tab\there.pdf", `quote".pdf`} {
		got := contentDisposition(name, true)
		star := got[strings.Index(got, marker)+len(marker):]
		for _, bad := range []string{";", ",", "=", `"`, " ", "\t"} {
			if strings.Contains(star, bad) {
				t.Errorf("contentDisposition(%q) ext-value %q contains raw %q", name, star, bad)
			}
		}
	}
}

// Non-ASCII survives in the ext-value and is replaced in the ASCII fallback.
func TestContentDispositionNonASCII(t *testing.T) {
	got := contentDisposition("rapport-café.pdf", false)
	if !strings.Contains(got, "filename*=UTF-8''rapport-caf%C3%A9.pdf") {
		t.Errorf("ext-value not UTF-8 percent-encoded: %s", got)
	}
	if !strings.Contains(got, `filename="rapport-caf_.pdf"`) {
		t.Errorf("ascii fallback not sanitised: %s", got)
	}
}

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

func TestParseUploadMetadata(t *testing.T) {
	// tus encodes values in base64 because the header must be ASCII and
	// filenames are not.
	got := parseUploadMetadata("filename 5pel5pys6KqeLnR4dA==,filetype dGV4dC9wbGFpbg==,is_final")
	if got["filename"] != "日本語.txt" {
		t.Errorf("filename = %q", got["filename"])
	}
	if got["filetype"] != "text/plain" {
		t.Errorf("filetype = %q", got["filetype"])
	}
	// A valueless pair is legal per the spec.
	if v, ok := got["is_final"]; !ok || v != "" {
		t.Errorf("valueless pair = %q, present=%t", v, ok)
	}
}

func TestParseUploadMetadataSkipsMalformedPairs(t *testing.T) {
	// One unparseable optional field must not stop an upload whose filename
	// decoded perfectly well.
	got := parseUploadMetadata("filename cmVwb3J0LnBkZg==,junk !!!not-base64!!!")
	if got["filename"] != "report.pdf" {
		t.Errorf("filename = %q, want report.pdf", got["filename"])
	}
	if _, ok := got["junk"]; ok {
		t.Error("a malformed pair was accepted")
	}
}

func TestParseUploadMetadataEmpty(t *testing.T) {
	if len(parseUploadMetadata("")) != 0 {
		t.Error("empty header should yield no metadata")
	}
}
