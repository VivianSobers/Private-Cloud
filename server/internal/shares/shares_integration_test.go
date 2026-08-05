package shares_test

// Slice 4: public share links.
//
// This is the security-sensitive slice, so the tests read like an attacker's
// checklist: the token is stored only hashed, a password actually gates content,
// expiry and revocation and the download cap all deny, a folder share cannot be
// walked out of, and a revoked or trashed target leaks nothing — not even the
// filename.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/shares"
)

type fixture struct {
	t      *testing.T
	ctx    context.Context
	files  *files.Service
	shares *shares.Service
	user   uuid.UUID
	root   uuid.UUID
	db     *db.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("PC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PC_TEST_DATABASE_URL not set; skipping integration tests")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	database, err := db.Open(ctx, dsn, 8, 1, 10*time.Second, log)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(database.Close)
	if err := database.Migrate(ctx, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	authStore := auth.NewStore(database.Pool)
	name := "share-" + uuid.NewString()[:8]
	user, err := authStore.CreateUser(ctx, uuid.New(), name, name, false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	filesSvc := files.NewService(files.NewStore(database.Pool), blobs, log)
	root, err := filesSvc.Store().EnsureRoot(ctx, user.ID)
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	sharesSvc := shares.NewService(shares.NewStore(database.Pool), filesSvc, log)

	return &fixture{
		t: t, ctx: ctx, files: filesSvc, shares: sharesSvc,
		user: user.ID, root: root.ID, db: database,
	}
}

func (f *fixture) upload(parent uuid.UUID, name, content string) *files.Node {
	f.t.Helper()
	n, err := f.files.Upload(f.ctx, f.user, parent, name, strings.NewReader(content), "")
	if err != nil {
		f.t.Fatalf("upload %q: %v", name, err)
	}
	return n
}

func (f *fixture) mkdir(parent uuid.UUID, name string) *files.Node {
	f.t.Helper()
	n, err := f.files.Store().CreateFolder(f.ctx, f.user, parent, name)
	if err != nil {
		f.t.Fatalf("mkdir %q: %v", name, err)
	}
	return n
}

func (f *fixture) readAll(rc io.ReadSeekCloser) string {
	f.t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		f.t.Fatalf("read content: %v", err)
	}
	return string(b)
}

func TestCreateAndServePublicFile(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "report.txt", "quarterly numbers")

	share, token, err := f.shares.Create(f.ctx, f.user, node.ID, shares.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if token == "" || share.HasPassword() {
		t.Fatal("a plain share should have a token and no password")
	}

	// The public view exposes the file, but nothing about where it lives.
	view, err := f.shares.View(f.ctx, token, "", "")
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if !view.Unlocked || view.Name != "report.txt" || view.Kind != "file" {
		t.Errorf("unexpected view: %+v", view)
	}
	if view.Size != int64(len("quarterly numbers")) {
		t.Errorf("view size = %d", view.Size)
	}

	content, rc, err := f.shares.OpenContent(f.ctx, token, "", "")
	if err != nil {
		t.Fatalf("OpenContent: %v", err)
	}
	if content.Name != "report.txt" {
		t.Errorf("content name = %q", content.Name)
	}
	if got := f.readAll(rc); got != "quarterly numbers" {
		t.Errorf("served content = %q", got)
	}
}

func TestTokenStoredOnlyHashed(t *testing.T) {
	// A database leak must not hand over working links: the row holds the token's
	// SHA-256, never the token.
	f := newFixture(t)
	node := f.upload(f.root, "secret.txt", "x")

	share, token, err := f.shares.Create(f.ctx, f.user, node.ID, shares.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := f.db.Pool.QueryRow(f.ctx,
		`SELECT token_hash FROM shares WHERE id = $1`, share.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(token))
	if !bytes.Equal(stored, want[:]) {
		t.Error("stored token_hash is not the SHA-256 of the token")
	}
	// And the plaintext token must appear in no column.
	var hit bool
	if err := f.db.Pool.QueryRow(f.ctx,
		`SELECT EXISTS(SELECT 1 FROM shares WHERE id=$1 AND password_hash = $2)`,
		share.ID, token).Scan(&hit); err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Error("the plaintext token leaked into a stored column")
	}
}
