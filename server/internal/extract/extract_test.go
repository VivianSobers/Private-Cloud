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

func TestExtractRejectsBinaryAndUnknown(t *testing.T) {
	e := New()

	// A binary payload mislabelled as text is not indexed as mojibake.
	if _, err := e.Extract(context.Background(), "text/plain", bytes.NewReader([]byte{0xff, 0xfe, 0x00, 0x01})); !errors.Is(err, ErrUnsupported) {
		t.Errorf("invalid utf-8 text: err = %v, want ErrUnsupported", err)
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
	yes := []string{"text/plain", "text/markdown", "application/json", "image/svg+xml", "application/ld+json"}
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
