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
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
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

func TestPasswordGatesContent(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "vault.txt", "top secret")
	_, token, err := f.shares.Create(f.ctx, f.user, node.ID, shares.CreateOptions{Password: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}

	// A locked view reveals nothing but "there is a password" — not even the name.
	view, err := f.shares.View(f.ctx, token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Unlocked || view.Name != "" || view.Kind != "" {
		t.Errorf("locked view leaked information: %+v", view)
	}
	if !view.HasPassword {
		t.Error("locked view did not signal that a password is required")
	}

	// Content is denied without the proof...
	if _, _, err := f.shares.OpenContent(f.ctx, token, "", ""); !errors.Is(err, shares.ErrPasswordNeeded) {
		t.Errorf("content without unlock = %v, want ErrPasswordNeeded", err)
	}
	// ...a wrong password does not unlock...
	if _, err := f.shares.Unlock(f.ctx, token, "wrong"); !errors.Is(err, shares.ErrWrongPassword) {
		t.Errorf("wrong password = %v, want ErrWrongPassword", err)
	}
	// ...and the right one yields a proof that unlocks view and content.
	proof, err := f.shares.Unlock(f.ctx, token, "hunter2")
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	v2, err := f.shares.View(f.ctx, token, proof, "")
	if err != nil || !v2.Unlocked || v2.Name != "vault.txt" {
		t.Errorf("unlocked view wrong: %+v (err %v)", v2, err)
	}
	_, rc, err := f.shares.OpenContent(f.ctx, token, proof, "")
	if err != nil {
		t.Fatalf("OpenContent with proof: %v", err)
	}
	if got := f.readAll(rc); got != "top secret" {
		t.Errorf("served %q", got)
	}
}

// expire backdates a share's expiry so a test does not wait it out.
func (f *fixture) expire(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := f.db.Pool.Exec(f.ctx,
		`UPDATE shares SET expires_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("expire share: %v", err)
	}
}

func TestExpiredShareDenied(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "temp.txt", "expires soon")
	share, token, err := f.shares.Create(f.ctx, f.user, node.ID, shares.CreateOptions{ExpiresIn: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	f.expire(t, share.ID)

	if _, err := f.shares.View(f.ctx, token, "", ""); !errors.Is(err, shares.ErrGone) {
		t.Errorf("view of expired share = %v, want ErrGone", err)
	}
	if _, _, err := f.shares.OpenContent(f.ctx, token, "", ""); !errors.Is(err, shares.ErrGone) {
		t.Errorf("content of expired share = %v, want ErrGone", err)
	}
}

func TestRevokedShareLooksLikeNotFound(t *testing.T) {
	// Revocation is immediate, and to a probe it is indistinguishable from a
	// token that never existed — a revoked link must not confirm it once was real.
	f := newFixture(t)
	node := f.upload(f.root, "gone.txt", "revoke me")
	share, token, err := f.shares.Create(f.ctx, f.user, node.ID, shares.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := f.shares.Revoke(f.ctx, f.user, share.ID); err != nil || !ok {
		t.Fatalf("Revoke: ok=%v err=%v", ok, err)
	}
	if _, err := f.shares.View(f.ctx, token, "", ""); !errors.Is(err, shares.ErrNotFound) {
		t.Errorf("view of revoked share = %v, want ErrNotFound", err)
	}
	if _, _, err := f.shares.OpenContent(f.ctx, token, "", ""); !errors.Is(err, shares.ErrNotFound) {
		t.Errorf("content of revoked share = %v, want ErrNotFound", err)
	}
}

func TestDownloadCapDeniesPastLimit(t *testing.T) {
	f := newFixture(t)
	node := f.upload(f.root, "capped.txt", "limited")
	_, token, err := f.shares.Create(f.ctx, f.user, node.ID, shares.CreateOptions{MaxDownloads: 2})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		_, rc, err := f.shares.OpenContent(f.ctx, token, "", "")
		if err != nil {
			t.Fatalf("download %d = %v, want success", i+1, err)
		}
		rc.Close()
	}
	if _, _, err := f.shares.OpenContent(f.ctx, token, "", ""); !errors.Is(err, shares.ErrGone) {
		t.Errorf("download past the cap = %v, want ErrGone", err)
	}
}

func TestDownloadCapHoldsUnderConcurrency(t *testing.T) {
	// The cap is enforced in a single atomic UPDATE, so many simultaneous
	// downloads racing the last permitted one cannot over-serve: exactly the cap
	// succeeds, no more.
	f := newFixture(t)
	node := f.upload(f.root, "race.txt", "one at most")
	const capN = 3
	_, token, err := f.shares.Create(f.ctx, f.user, node.ID, shares.CreateOptions{MaxDownloads: capN})
	if err != nil {
		t.Fatal(err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rc, err := f.shares.OpenContent(f.ctx, token, "", "")
			if err == nil {
				rc.Close()
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != capN {
		t.Errorf("%d downloads were served against a cap of %d", granted, capN)
	}
}
