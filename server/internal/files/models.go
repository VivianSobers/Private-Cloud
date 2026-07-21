// Package files owns the user-visible file tree: folders, files, versions and
// the trash.
//
// Two invariants shape everything here:
//
//   - The BYTES ARE WRITTEN BEFORE THE ROW. A crash between the two leaves an
//     orphan blob, which fsck reclaims. The reverse ordering would leave a row
//     pointing at content that does not exist, which no amount of sweeping can
//     repair.
//   - PATHS ARE MATERIALISED. nodes.path is denormalised from the parent chain
//     so subtree reads are a prefix scan. Rename pays for it by rewriting
//     descendants, inside the same transaction that renames the node.
package files

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

var (
	ErrNotFound     = errors.New("node not found")
	ErrNotAFolder   = errors.New("parent is not a folder")
	ErrNotAFile     = errors.New("node is not a file")
	ErrNameTaken    = errors.New("a node with that name already exists here")
	ErrInvalidName  = errors.New("invalid name")
	ErrCycle        = errors.New("cannot move a folder into itself")
	ErrQuota        = errors.New("quota exceeded")
	ErrRootReserved = errors.New("the root folder cannot be renamed, moved or trashed")
	ErrNotTrashed   = errors.New("node is not in the trash")
)

const (
	KindFolder = "folder"
	KindFile   = "file"
)

// Node is one entry in the tree. A folder has no head version; a file has
// exactly one (slice 3 keeps no history — the table shape allows it later).
type Node struct {
	ID       uuid.UUID
	OwnerID  uuid.UUID
	ParentID *uuid.UUID
	Kind     string
	Name     string
	Path     string

	HeadVersionID *uuid.UUID
	TrashedAt     *time.Time
	// TrashedRootID is the node the user actually deleted. It equals ID on the
	// top of a deleted subtree and points at that top on everything below.
	TrashedRootID *uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Populated for files, from the head version. Zero for folders.
	Size int64
	MIME string
	// SHA256 of the whole file, used for the download ETag.
	SHA256 []byte
	// BlobKey is never exposed over the API — it is storage-layer detail that
	// would leak the on-disk layout.
	BlobKey string
}

func (n *Node) IsFolder() bool  { return n.Kind == KindFolder }
func (n *Node) IsFile() bool    { return n.Kind == KindFile }
func (n *Node) IsTrashed() bool { return n.TrashedAt != nil }
func (n *Node) IsRoot() bool    { return n.ParentID == nil }

// Usage is the quota picture for one user.
type Usage struct {
	// LiveBytes counts files outside the trash.
	LiveBytes int64
	// TrashBytes counts files in the trash. Reported separately because
	// "delete something to free space" is only true once the trash is emptied,
	// and hiding that makes the number look like a lie.
	TrashBytes int64
	FileCount  int64
	QuotaBytes *int64
}

func (u Usage) TotalBytes() int64 { return u.LiveBytes + u.TrashBytes }

// --- names ------------------------------------------------------------------

// maxNameLen matches the practical limit on ext4/ZFS (255 bytes). Enforcing it
// here means an export to a real filesystem in a later phase cannot fail on
// names this server happily accepted.
const maxNameLen = 255

// ValidateName rejects names that would be unusable, ambiguous or dangerous on
// at least one of the platforms this tree has to survive: the API, WebDAV
// clients from Windows and macOS, and eventually a real filesystem export.
func ValidateName(name string) error {
	if name == "" {
		return ErrInvalidName
	}
	if len(name) > maxNameLen {
		return ErrInvalidName
	}
	// "." and ".." would make the materialised path ambiguous and are how a
	// path traversal would be spelled if one ever got this far.
	if name == "." || name == ".." {
		return ErrInvalidName
	}
	if strings.ContainsAny(name, "/\\") {
		return ErrInvalidName
	}
	// Control characters, including NUL, break C-string-based clients and
	// terminal output alike.
	for _, r := range name {
		if unicode.IsControl(r) {
			return ErrInvalidName
		}
	}
	// Windows reserves these entirely, and trailing dots/spaces are silently
	// stripped by its filesystem — which would turn "report ." and "report"
	// into a collision only Windows users experience.
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return ErrInvalidName
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return ErrInvalidName
	}
	if isWindowsReserved(name) {
		return ErrInvalidName
	}
	return nil
}

// isWindowsReserved matches CON, PRN, AUX, NUL, COM1-9, LPT1-9, with or without
// an extension. Windows refuses to create these at all.
func isWindowsReserved(name string) bool {
	base := name
	if i := strings.IndexByte(name, '.'); i > 0 {
		base = name[:i]
	}
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	upper := strings.ToUpper(base)
	if len(upper) == 4 {
		prefix := upper[:3]
		last := upper[3]
		if (prefix == "COM" || prefix == "LPT") && last >= '1' && last <= '9' {
			return true
		}
	}
	return false
}

// Fold produces the key sibling uniqueness is enforced on.
//
// Simple lowercasing, not full Unicode case folding or NFC normalisation.
// That is a deliberate limit: it catches the case that actually happens
// ("Photos" vs "photos" from a macOS or Windows client) without pretending to
// solve the harder problem of "é" composed two different ways, which needs
// golang.org/x/text/unicode/norm and a decision about which form to store.
// Phase 2 revisits this alongside the sync engine, where it matters more.
func Fold(name string) string { return strings.ToLower(name) }

// JoinPath builds a child path. The root's path is "/", so a naive
// parent+"/"+name would produce "//photos".
func JoinPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}
