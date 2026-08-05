package shares

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"path"
	"strings"
	"time"

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
