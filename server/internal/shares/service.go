package shares

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Errors the public surface maps to status codes. They are deliberately coarse:
// a caller learns "no" and, at most, "you need the password", never anything
// that distinguishes a revoked link from one that never existed.
var (
	ErrNotFound        = errors.New("share not found")
	ErrGone            = errors.New("share is no longer available")
	ErrPasswordNeeded  = errors.New("share requires a password")
	ErrWrongPassword   = errors.New("incorrect password")
	ErrNotAFile        = errors.New("not a file")
	ErrShareTargetKind = errors.New("only files and folders can be shared")
)

// Service is the share plane's logic. It holds files.Service because serving a
// share means opening one user's content on behalf of an anonymous visitor —
// the share row IS the authorisation, standing in for the owner.
type Service struct {
	store *Store
	files *files.Service
	log   *slog.Logger

	// StaleGrace is how long a revoked or expired share lingers before GC removes
	// the row, so the owner's list can still show what they revoked.
	StaleGrace time.Duration
}

func NewService(store *Store, filesSvc *files.Service, log *slog.Logger) *Service {
	return &Service{store: store, files: filesSvc, log: log, StaleGrace: 24 * time.Hour}
}

func (s *Service) Store() *Store { return s.store }

// tokenBytes is the entropy of a share token: 256 bits, unguessable, so the URL
// itself is the primary credential.
const tokenBytes = 32

// CreateOptions are the knobs a share can be born with. Zero values mean "no
// password", "never expires", "no download cap" — the least restrictive link.
type CreateOptions struct {
	Password     string
	ExpiresIn    time.Duration
	MaxDownloads int64
}

// Create mints a share for one of the owner's files or folders and returns the
// plaintext token ONCE. The token is never stored — only its hash — so it cannot
// be recovered later; a lost link is re-created, not looked up.
func (s *Service) Create(ctx context.Context, ownerID, nodeID uuid.UUID, opts CreateOptions) (*Share, string, error) {
	node, err := s.files.Store().GetLive(ctx, ownerID, nodeID)
	if err != nil {
		// files.ErrNotFound: not the owner's, or not live. The HTTP layer maps it.
		return nil, "", err
	}
	// The root is the whole account; sharing it would turn one link into blanket
	// access to everything the user owns. A share names a file or a folder.
	if node.IsRoot() {
		return nil, "", ErrShareTargetKind
	}
	if !node.IsFile() && !node.IsFolder() {
		return nil, "", ErrShareTargetKind
	}

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	unlockKey := make([]byte, 32)
	if _, err := rand.Read(unlockKey); err != nil {
		return nil, "", err
	}

	var pwHash string
	if opts.Password != "" {
		if pwHash, err = auth.HashSecret(opts.Password); err != nil {
			return nil, "", err
		}
	}
	var expiresAt *time.Time
	if opts.ExpiresIn > 0 {
		t := time.Now().Add(opts.ExpiresIn)
		expiresAt = &t
	}
	var maxDownloads *int64
	if opts.MaxDownloads > 0 {
		m := opts.MaxDownloads
		maxDownloads = &m
	}

	share, err := s.store.Create(ctx, CreateInput{
		NodeID:       node.ID,
		OwnerID:      ownerID,
		TokenHash:    hashToken(token),
		UnlockKey:    unlockKey,
		PasswordHash: pwHash,
		ExpiresAt:    expiresAt,
		MaxDownloads: maxDownloads,
	})
	if err != nil {
		return nil, "", err
	}
	return share, token, nil
}

// List returns the owner's shares. Revoke kills one immediately.
func (s *Service) List(ctx context.Context, ownerID uuid.UUID) ([]OwnerShare, error) {
	return s.store.ListForOwner(ctx, ownerID)
}

func (s *Service) Revoke(ctx context.Context, ownerID, id uuid.UUID) (bool, error) {
	return s.store.Revoke(ctx, ownerID, id)
}

// lookup resolves a presented token to its share, collapsing a missing row into
// ErrNotFound so the public surface answers typos and revocations identically.
func (s *Service) lookup(ctx context.Context, token string) (*Share, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	return s.store.FindByTokenHash(ctx, hashToken(token))
}

// Unlock verifies a password and returns the proof the content endpoint will
// require. For a passwordless share it succeeds trivially — the link alone is
// the credential — so a client can call it uniformly without branching.
func (s *Service) Unlock(ctx context.Context, token, password string) (string, error) {
	sh, err := s.lookup(ctx, token)
	if err != nil {
		return "", err
	}
	if err := checkValidity(sh, time.Now()); err != nil {
		return "", err
	}
	if sh.HasPassword() && !auth.VerifySecret(password, sh.PasswordHash) {
		return "", ErrWrongPassword
	}
	return unlockProof(sh), nil
}

// Entry is one child of a shared folder, carrying only what a listing needs —
// name, kind, size. Never a path or an id: the visitor navigates by name within
// the share, and the rest of the owner's tree stays invisible.
type Entry struct {
	Name string
	Kind string
	Size int64
}

// PublicShare is the entire leak-free view of a share. When a password gates it
// and the caller has not unlocked, it carries nothing but HasPassword — not even
// the filename, so the second gate the owner asked for actually gates something.
type PublicShare struct {
	HasPassword bool
	Unlocked    bool

	Name    string
	Kind    string // "file" | "folder"
	Size    int64
	MIME    string
	Path    string  // relative sub-path being viewed within a folder share
	Entries []Entry // populated for an unlocked folder
}

// View returns what the landing page may show. A locked share reveals only that
// a password is needed; an unlocked (or passwordless) one reveals the file's
// size or the folder's immediate listing at relPath — and never the owner, the
// absolute path, or anything outside the shared subtree.
func (s *Service) View(ctx context.Context, token, proof, relPath string) (*PublicShare, error) {
	sh, err := s.lookup(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := checkValidity(sh, time.Now()); err != nil {
		return nil, err
	}

	if sh.HasPassword() && !validUnlock(sh, proof) {
		// Locked: the minimum that lets a client know to prompt for a password.
		return &PublicShare{HasPassword: true}, nil
	}

	node, err := s.resolveNode(ctx, sh, relPath)
	if err != nil {
		return nil, err
	}

	pv := &PublicShare{
		HasPassword: sh.HasPassword(),
		Unlocked:    true,
		Name:        node.Name,
		Kind:        node.Kind,
		Path:        strings.Trim(relPath, "/"),
	}
	if node.IsFile() {
		pv.Size = node.Size
		pv.MIME = node.MIME
		return pv, nil
	}

	children, err := s.files.Store().ListChildren(ctx, sh.OwnerID, node.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range children {
		pv.Entries = append(pv.Entries, Entry{Name: c.Name, Kind: c.Kind, Size: c.Size})
	}
	return pv, nil
}

// hashToken hashes a URL token for lookup. SHA-256 is right here for the same
// reason it is for session tokens: the token is 256 bits of CSPRNG output, so
// there is no low-entropy secret to slow an attacker over, and hashing at rest
// keeps a database leak from yielding working links.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// unlockProof is the value handed back after a password is verified: an HMAC
// over the share id, keyed by the share's server-only unlock_key. A visitor
// cannot forge it without the key, and the key never leaves the database — so
// possession of a valid proof is proof the password was checked, with no
// per-visitor session state to store.
func unlockProof(sh *Share) string {
	mac := hmac.New(sha256.New, sh.UnlockKey)
	mac.Write([]byte(sh.ID.String()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validUnlock reports whether proof matches, in constant time.
func validUnlock(sh *Share, proof string) bool {
	return subtle.ConstantTimeCompare([]byte(proof), []byte(unlockProof(sh))) == 1
}

// resolveNode maps a share and a path relative to its shared root to the live
// node being addressed, refusing anything that escapes the shared subtree.
//
// Two independent guards keep a folder share from leaking the rest of the tree:
// GetByPath is scoped to the owner (so at worst a traversal reaches the owner's
// own files), and the prefix check then confines the result to the shared
// subtree. path.Join normalises "../" before either runs, so an escaping path is
// already collapsed by the time it is looked up.
func (s *Service) resolveNode(ctx context.Context, sh *Share, relPath string) (*files.Node, error) {
	root, err := s.files.Store().GetLive(ctx, sh.OwnerID, sh.NodeID)
	if err != nil {
		// The shared node was trashed or purged: the link is dead, indistinguishable
		// from one that never existed.
		return nil, ErrNotFound
	}

	rel := strings.Trim(relPath, "/")
	if rel == "" {
		return root, nil
	}
	if !root.IsFolder() {
		// A file share has no sub-paths to address.
		return nil, ErrNotFound
	}

	abs := path.Join(root.Path, rel)
	node, err := s.files.Store().GetByPath(ctx, sh.OwnerID, abs)
	if err != nil {
		return nil, ErrNotFound
	}
	// Confinement: the resolved node must be the shared root or live beneath it.
	if node.Path != root.Path && !strings.HasPrefix(node.Path, root.Path+"/") {
		return nil, ErrNotFound
	}
	return node, nil
}

// checkValidity turns a share's state into an error, against one clock reading so
// the reasons cannot disagree with each other.
func checkValidity(sh *Share, now time.Time) error {
	if sh.Revoked() {
		// Revoked looks like never-existed to the public: 404, not a distinct code.
		return ErrNotFound
	}
	if sh.Expired(now) || sh.CapReached() {
		return ErrGone
	}
	return nil
}
