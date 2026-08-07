package extract

import (
	"slices"
	"testing"
)

func TestTagsMimeCategories(t *testing.T) {
	cases := map[string]string{
		"image/png":       "image",
		"image/jpeg":      "image",
		"video/mp4":       "video",
		"audio/mpeg":      "audio",
		"application/pdf": "pdf",
		"text/plain":      "text",
		"text/csv":        "spreadsheet",
		"application/zip": "archive",
	}
	for mime, want := range cases {
		got := Tags(mime, "")
		if !slices.Contains(got, want) {
			t.Errorf("Tags(%q) = %v, want to contain %q", mime, got, want)
		}
	}
	// A PDF is both a document and a pdf.
	pdf := Tags("application/pdf", "")
	if !slices.Contains(pdf, "document") || !slices.Contains(pdf, "pdf") {
		t.Errorf("pdf tags = %v, want document+pdf", pdf)
	}
}

func TestTagsFromKeywords(t *testing.T) {
	tags := Tags("text/plain", "INVOICE #4471\nSubtotal: 40.00\nSales tax: 2.00\nAmount due: 42.00")
	for _, want := range []string{"invoice", "financial", "text"} {
		if !slices.Contains(tags, want) {
			t.Errorf("tags = %v, want to contain %q", tags, want)
		}
	}
}

func TestTagsAreSortedAndDeduped(t *testing.T) {
	tags := Tags("application/pdf", "This receipt is your order confirmation. Receipt total below.")
	// Sorted.
	if !slices.IsSorted(tags) {
		t.Errorf("tags not sorted: %v", tags)
	}
	// No duplicates even though "receipt" triggers twice.
	seen := map[string]bool{}
	for _, tag := range tags {
		if seen[tag] {
			t.Errorf("duplicate tag %q in %v", tag, tags)
		}
		seen[tag] = true
	}
}

func TestTagsEmptyForUnknownAndBlank(t *testing.T) {
	if got := Tags("application/octet-stream", ""); len(got) != 0 {
		t.Errorf("unknown type with no text should have no tags, got %v", got)
	}
}
