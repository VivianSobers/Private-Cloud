package files_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

func tagNames(tags []files.Tag) []string {
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = t.Name
	}
	return out
}

// Extraction auto-tags the file it processes: MIME category plus any keyword
// matches, all as 'auto' tags.
func TestExtractionAutoTags(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	h := extract.NewHandler(files.NewExtractOpener(f.svc), f.store, extract.New(), nil)
	h.Tagging(f.store)

	node, err := f.svc.Upload(ctx, f.user, f.root, "bill.txt",
		strings.NewReader("INVOICE #900\nSubtotal 10.00\nSales tax 0.80\nAmount due 10.80"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Handle(ctx, &node.ID, f.user); err != nil {
		t.Fatal(err)
	}

	tags, err := f.store.TagsForNode(ctx, f.user, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := tagNames(tags)
	for _, want := range []string{"text", "invoice", "financial"} {
		if !slices.Contains(names, want) {
			t.Errorf("auto tags %v missing %q", names, want)
		}
	}
	for _, tg := range tags {
		if tg.Source != "auto" {
			t.Errorf("tag %q source = %q, want auto", tg.Name, tg.Source)
		}
	}
}

// User tags survive re-tagging; auto tags are replaced by it.
func TestUserTagsSurviveReTagging(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	node, err := f.svc.Upload(ctx, f.user, f.root, "note.txt", strings.NewReader("hello"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	// A user tag and an initial auto set.
	if err := f.store.AddUserTag(ctx, f.user, node.ID, "Important"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetAutoTags(ctx, node.ID, []string{"text", "draft"}); err != nil {
		t.Fatal(err)
	}

	// Re-tag with a different auto set: "draft" goes, the user tag stays.
	if err := f.store.SetAutoTags(ctx, node.ID, []string{"text"}); err != nil {
		t.Fatal(err)
	}
	tags, _ := f.store.TagsForNode(ctx, f.user, node.ID)
	names := tagNames(tags)
	if !slices.Contains(names, "important") { // normalized to lowercase
		t.Errorf("user tag lost: %v", names)
	}
	if slices.Contains(names, "draft") {
		t.Errorf("stale auto tag survived re-tagging: %v", names)
	}

	// Removing the user tag works; a wrong owner cannot touch it.
	other := newFixture(t)
	if err := other.store.RemoveTag(ctx, other.user, node.ID, "important"); err == nil {
		t.Error("a different user removed a tag they do not own")
	}
	if err := f.store.RemoveTag(ctx, f.user, node.ID, "important"); err != nil {
		t.Fatal(err)
	}
	tags, _ = f.store.TagsForNode(ctx, f.user, node.ID)
	if slices.Contains(tagNames(tags), "important") {
		t.Error("tag not removed")
	}
}

// Listing tags counts live files, and filtering by tag returns them.
func TestListAndFilterByTag(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	a, _ := f.svc.Upload(ctx, f.user, f.root, "a.txt", strings.NewReader("a"), "text/plain")
	b, _ := f.svc.Upload(ctx, f.user, f.root, "b.txt", strings.NewReader("b"), "text/plain")
	if err := f.store.AddUserTag(ctx, f.user, a.ID, "project-x"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddUserTag(ctx, f.user, b.ID, "project-x"); err != nil {
		t.Fatal(err)
	}

	counts, err := f.store.ListTags(ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	var found int64
	for _, c := range counts {
		if c.Tag == "project-x" {
			found = c.Count
		}
	}
	if found != 2 {
		t.Errorf("project-x count = %d, want 2", found)
	}

	nodes, err := f.store.NodesByTag(ctx, f.user, "project-x", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Errorf("nodes by tag = %d, want 2", len(nodes))
	}
}

// A tag carrying a control character is refused — it would be an injection vector
// once echoed back on display.
func TestAddTagRejectsControlChars(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	node, err := f.svc.Upload(ctx, f.user, f.root, "x.txt", strings.NewReader("x"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddUserTag(ctx, f.user, node.ID, "bad\x00tag"); err == nil {
		t.Error("tag with a NUL byte was accepted")
	}
	if err := f.store.AddUserTag(ctx, f.user, node.ID, "line\nbreak"); err == nil {
		t.Error("tag with a newline was accepted")
	}
}
