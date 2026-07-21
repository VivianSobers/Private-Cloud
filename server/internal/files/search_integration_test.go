package files_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Search is trigram matching over names and paths, so the tests that matter are
// the ones a full-text implementation would fail: mid-word fragments, digits
// inside a filename, and ranking.

func (f *fixture) search(q files.SearchQuery) []*files.SearchResult {
	f.t.Helper()
	res, err := f.store.Search(f.ctx, f.user, q)
	if err != nil {
		f.t.Fatalf("Search(%+v): %v", q, err)
	}
	return res
}

func names(results []*files.SearchResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Name)
	}
	return out
}

func hasName(results []*files.SearchResult, name string) bool {
	for _, r := range results {
		if r.Name == name {
			return true
		}
	}
	return false
}

func TestSearchFindsMidWordFragments(t *testing.T) {
	// The reason this is trigram search and not full-text search. "budg" is not
	// a token in "budget-2026-final.xlsx", and to_tsvector would find nothing.
	f := newFixture(t)
	f.upload(f.root, "budget-2026-final.xlsx", "x")
	f.upload(f.root, "unrelated.txt", "x")

	for _, frag := range []string{"budg", "2026", "final", "xlsx", "et-20"} {
		got := f.search(files.SearchQuery{Text: frag})
		if !hasName(got, "budget-2026-final.xlsx") {
			t.Errorf("search(%q) = %v, want the budget file", frag, names(got))
		}
	}
}

func TestSearchIsCaseInsensitive(t *testing.T) {
	f := newFixture(t)
	f.upload(f.root, "Quarterly Report.PDF", "x")

	for _, q := range []string{"quarterly", "QUARTERLY", "Report", "pdf", "PDF"} {
		if got := f.search(files.SearchQuery{Text: q}); len(got) != 1 {
			t.Errorf("search(%q) returned %v, want the one report", q, names(got))
		}
	}
}

func TestSearchMatchesPathAsWellAsName(t *testing.T) {
	// So "photos" finds everything inside /photos without the caller having to
	// know whether the fragment spans a directory boundary.
	f := newFixture(t)
	photos := f.mkdir(f.root, "photos")
	y2026 := f.mkdir(photos.ID, "2026")
	f.upload(y2026.ID, "IMG_0001.jpg", "x")
	f.upload(f.root, "elsewhere.jpg", "x")

	got := f.search(files.SearchQuery{Text: "photos"})
	if !hasName(got, "IMG_0001.jpg") {
		t.Errorf("search(photos) = %v, want the nested image", names(got))
	}
	if hasName(got, "elsewhere.jpg") {
		t.Errorf("search(photos) matched a file outside /photos: %v", names(got))
	}

	// And the result says WHY it matched, so a filename with no visible
	// relationship to the query does not look like a bug.
	for _, r := range got {
		if r.Name == "IMG_0001.jpg" && !r.MatchedPath {
			t.Error("a path-only match was not flagged as one")
		}
		if r.Name == "photos" && r.MatchedPath {
			t.Error("a name match was wrongly flagged as a path match")
		}
	}
}

func TestSearchRanksExactThenPrefixThenSimilarity(t *testing.T) {
	// Someone typing a full filename wants that file, not the forty others
	// containing it as a substring.
	f := newFixture(t)
	f.upload(f.root, "old-budget-archive.xlsx", "x")
	f.upload(f.root, "budget.xlsx", "x")
	f.upload(f.root, "budget", "x")

	got := f.search(files.SearchQuery{Text: "budget"})
	if len(got) < 3 {
		t.Fatalf("search returned %v, want all three", names(got))
	}
	if got[0].Name != "budget" {
		t.Errorf("first result = %q, want the exact match", got[0].Name)
	}
	if got[1].Name != "budget.xlsx" {
		t.Errorf("second result = %q, want the prefix match", got[1].Name)
	}
}

func TestSearchExcludesTrashByDefault(t *testing.T) {
	// Finding a file you deleted last month and being unable to tell it is
	// deleted is worse than not finding it.
	f := newFixture(t)
	keep := f.upload(f.root, "report-keep.txt", "x")
	gone := f.upload(f.root, "report-gone.txt", "x")

	if _, err := f.store.Trash(f.ctx, f.user, gone.ID); err != nil {
		t.Fatal(err)
	}

	got := f.search(files.SearchQuery{Text: "report"})
	if len(got) != 1 || got[0].ID != keep.ID {
		t.Errorf("search = %v, want only the live file", names(got))
	}

	got = f.search(files.SearchQuery{Text: "report", IncludeTrashed: true})
	if len(got) != 2 {
		t.Errorf("search with IncludeTrashed = %v, want both", names(got))
	}
}

func TestSearchFiltersByKind(t *testing.T) {
	f := newFixture(t)
	f.mkdir(f.root, "invoices")
	f.upload(f.root, "invoices.csv", "x")

	if got := f.search(files.SearchQuery{Text: "invoice", Kind: "folder"}); len(got) != 1 || !got[0].IsFolder() {
		t.Errorf("kind=folder returned %v", names(got))
	}
	if got := f.search(files.SearchQuery{Text: "invoice", Kind: "file"}); len(got) != 1 || !got[0].IsFile() {
		t.Errorf("kind=file returned %v", names(got))
	}
	if got := f.search(files.SearchQuery{Text: "invoice"}); len(got) != 2 {
		t.Errorf("unfiltered returned %v, want both", names(got))
	}
}

func TestSearchScopedToSubtree(t *testing.T) {
	f := newFixture(t)
	work := f.mkdir(f.root, "work")
	personal := f.mkdir(f.root, "personal")
	f.upload(work.ID, "notes.txt", "x")
	f.upload(personal.ID, "notes.txt", "x")

	got := f.search(files.SearchQuery{Text: "notes", Under: "/work"})
	if len(got) != 1 {
		t.Fatalf("scoped search returned %v, want one", names(got))
	}
	if got[0].Path != "/work/notes.txt" {
		t.Errorf("scoped search returned %q", got[0].Path)
	}

	// A trailing slash is how clients spell a directory; it must not change
	// the result.
	if got := f.search(files.SearchQuery{Text: "notes", Under: "/work/"}); len(got) != 1 {
		t.Errorf("trailing slash changed the result: %v", names(got))
	}
}

func TestSearchNeverCrossesUsers(t *testing.T) {
	a := newFixture(t)
	b := newFixture(t)

	a.upload(a.root, "confidential-salaries.xlsx", "x")
	b.upload(b.root, "confidential-nothing.txt", "x")

	got, err := b.store.Search(b.ctx, b.user, files.SearchQuery{Text: "confidential"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Name == "confidential-salaries.xlsx" {
			t.Fatal("search crossed into another user's tree")
		}
	}
}

func TestSearchRejectsTooShortQueries(t *testing.T) {
	// pg_trgm indexes trigrams: a single character cannot use the index and
	// degrades to a sequential scan of the whole tree.
	f := newFixture(t)
	f.upload(f.root, "anything.txt", "x")

	for _, q := range []string{"", " ", "a", "  x  "} {
		if _, err := f.store.Search(f.ctx, f.user, files.SearchQuery{Text: q}); err == nil {
			t.Errorf("search(%q) was accepted; want a rejection", q)
		}
	}
}

func TestSearchExcludesTheRoot(t *testing.T) {
	// The root has an empty name; matching it would put a nameless entry at the
	// top of every result list, since '' is a substring of everything.
	f := newFixture(t)
	f.upload(f.root, "thing.txt", "x")

	for _, r := range f.search(files.SearchQuery{Text: "thing"}) {
		if r.IsRoot() {
			t.Error("the root appeared in search results")
		}
	}
}

func TestSearchHandlesWildcardsLiterally(t *testing.T) {
	// A user searching for "100%" must not have it treated as a LIKE wildcard
	// that matches every file they own.
	f := newFixture(t)
	f.upload(f.root, "100% done.txt", "x")
	f.upload(f.root, "unrelated.txt", "x")

	got := f.search(files.SearchQuery{Text: "100%"})
	if len(got) != 1 || got[0].Name != "100% done.txt" {
		t.Errorf("search(100%%) = %v, want only the literal match", names(got))
	}

	got = f.search(files.SearchQuery{Text: "_n"})
	for _, r := range got {
		if !strings.Contains(files.Fold(r.Name), "_n") && !strings.Contains(files.Fold(r.Path), "_n") {
			t.Errorf("underscore was treated as a wildcard: matched %q", r.Name)
		}
	}
}

func TestSearchPaging(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"page-a.txt", "page-b.txt", "page-c.txt", "page-d.txt"} {
		f.upload(f.root, n, "x")
	}

	first := f.search(files.SearchQuery{Text: "page", Limit: 2})
	if len(first) != 2 {
		t.Fatalf("first page = %v, want 2", names(first))
	}
	second := f.search(files.SearchQuery{Text: "page", Limit: 2, Offset: 2})
	if len(second) != 2 {
		t.Fatalf("second page = %v, want 2", names(second))
	}

	// The pages must not overlap, or paging silently loses results.
	seen := map[uuid.UUID]bool{}
	for _, r := range append(first, second...) {
		if seen[r.ID] {
			t.Errorf("node %q appeared on both pages", r.Name)
		}
		seen[r.ID] = true
	}
}

func TestSearchLimitIsCapped(t *testing.T) {
	// An unbounded limit is a way to ask the server to materialise the whole
	// tree in one response.
	f := newFixture(t)
	f.upload(f.root, "capped.txt", "x")

	if _, err := f.store.Search(f.ctx, f.user, files.SearchQuery{Text: "capped", Limit: 100000}); err != nil {
		t.Fatalf("an over-large limit should be clamped, not rejected: %v", err)
	}
}
