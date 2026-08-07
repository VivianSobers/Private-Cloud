package extract

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExtractPlainText(t *testing.T) {
	e := New()
	res, err := e.Extract(context.Background(), "text/plain; charset=utf-8",
		strings.NewReader("  Invoice #4471\nTotal: $19.90  "))
	if err != nil {
		t.Fatalf("extract text: %v", err)
	}
	if res.Source != "text" {
		t.Errorf("source = %q, want text", res.Source)
	}
	if !strings.Contains(res.Text, "Invoice #4471") || strings.HasPrefix(res.Text, " ") {
		t.Errorf("text not cleaned/extracted: %q", res.Text)
	}
}

func TestExtractMarkupIndexesTextNotTags(t *testing.T) {
	e := New()

	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="200">` +
		`<title>Quarterly revenue</title><text x="10">Northwind Traders</text></svg>`
	res, err := e.Extract(context.Background(), "image/svg+xml", strings.NewReader(svg))
	if err != nil {
		t.Fatalf("svg: %v", err)
	}
	if res.Source != "markup" {
		t.Errorf("source = %q, want markup", res.Source)
	}
	// The words a person would search for survive...
	for _, want := range []string{"Quarterly revenue", "Northwind Traders"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text %q missing %q", res.Text, want)
		}
	}
	// ...and the markup that nobody searches for does not.
	for _, unwanted := range []string{"svg", "xmlns", "http://www.w3.org", "width"} {
		if strings.Contains(res.Text, unwanted) {
			t.Errorf("text %q must not contain markup %q", res.Text, unwanted)
		}
	}
}

func TestExtractMarkupWithNoTextIsNoText(t *testing.T) {
	e := New()
	_, err := e.Extract(context.Background(), "application/xml",
		strings.NewReader(`<a><b attr="x"/></a>`))
	if !errors.Is(err, ErrNoText) {
		t.Errorf("tags-only xml: err = %v, want ErrNoText", err)
	}
}

func TestExtractRejectsBinaryAndUnknown(t *testing.T) {
	e := New()

	// A binary payload mislabelled as text is not indexed as mojibake.
	// An extractor ran and this type is supported; there is just nothing to index.
	if _, err := e.Extract(context.Background(), "text/plain", bytes.NewReader([]byte{0xff, 0xfe, 0x00, 0x01})); !errors.Is(err, ErrNoText) {
		t.Errorf("invalid utf-8 text: err = %v, want ErrNoText", err)
	}
	// An unhandled type has nothing to extract.
	if _, err := e.Extract(context.Background(), "application/octet-stream", strings.NewReader("x")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("octet-stream: err = %v, want ErrUnsupported", err)
	}
	// Empty text is nothing worth indexing.
	if _, err := e.Extract(context.Background(), "text/plain", strings.NewReader("   \n  ")); !errors.Is(err, ErrNoText) {
		t.Errorf("blank text: err = %v, want ErrNoText", err)
	}
}

// A malformed PDF must be a skip, never a worker crash — the recover guard is the
// point of this test.
func TestExtractMalformedPDFDoesNotPanic(t *testing.T) {
	e := New()
	_, err := e.Extract(context.Background(), "application/pdf", strings.NewReader("%PDF-1.4 not really a pdf"))
	if !errors.Is(err, ErrNoText) {
		t.Errorf("malformed pdf: err = %v, want ErrNoText", err)
	}
}

// Image extraction reports cleanly when OCR is unavailable, so a deployment
// without tesseract still runs. When tesseract IS present, a synthetic non-image
// still fails gracefully rather than panicking.
func TestExtractImageWithoutOCR(t *testing.T) {
	e := New()
	_, err := e.Extract(context.Background(), "image/png", strings.NewReader("not really an image"))
	if !e.HasOCR() {
		if !errors.Is(err, ErrNoOCR) {
			t.Errorf("no tesseract: err = %v, want ErrNoOCR", err)
		}
		return
	}
	// With OCR available, tesseract rejects the bogus image; that surfaces as an
	// error, not a panic or a false result.
	if err == nil {
		t.Error("expected an error OCR'ing non-image bytes")
	}
}

func TestIsTextual(t *testing.T) {
	// image/svg+xml is no longer here: the +xml family routes through isMarkup so
	// its tags are stripped before indexing. It is still text, just not decoded
	// verbatim. See TestIsMarkup.
	yes := []string{"text/plain", "text/markdown", "application/json", "application/ld+json"}
	no := []string{"image/png", "application/pdf", "application/octet-stream", "video/mp4"}
	for _, m := range yes {
		if !isTextual(m) {
			t.Errorf("%q should be textual", m)
		}
	}
	for _, m := range no {
		if isTextual(m) {
			t.Errorf("%q should not be textual", m)
		}
	}
}

func TestIsMarkup(t *testing.T) {
	yes := []string{"image/svg+xml", "application/xml", "text/xml", "text/html", "application/xhtml+xml"}
	no := []string{"text/plain", "application/json", "application/ld+json", "image/png"}
	for _, m := range yes {
		if !isMarkup(m) {
			t.Errorf("%q should be markup", m)
		}
	}
	for _, m := range no {
		if isMarkup(m) {
			t.Errorf("%q should not be markup", m)
		}
	}
	// Markup is checked before the image branch, so an SVG reaches the text path
	// rather than OCR — tesseract cannot read a vector file.
	if !isMarkup("image/svg+xml") || isTextual("image/svg+xml") {
		t.Error("image/svg+xml must route to markup, not to the plain text decoder")
	}
}

func TestTruncateTextRuneSafe(t *testing.T) {
	// A long run of a multibyte rune, truncated, must stay valid UTF-8.
	long := strings.Repeat("é", MaxTextBytes)
	out := truncateText(long)
	if len(out) > MaxTextBytes {
		t.Errorf("not truncated: %d bytes", len(out))
	}
	if !isValidUTF8(out) {
		t.Error("truncation split a multibyte rune")
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
