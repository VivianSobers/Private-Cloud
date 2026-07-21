// Package webdavfs adapts the file tree to golang.org/x/net/webdav.
//
// WebDAV exists so the cloud can be mounted as a network drive in Finder,
// Explorer, Nautilus or rclone — no client to install, no sync daemon, and
// every application on the machine can open files directly. It is the cheapest
// possible "works with everything" surface, and it is why the node store grew a
// path-addressed API in slice 3.
//
// The adapter is deliberately thin. Everything about ownership, naming rules,
// quota and trash already lives in files.Store; duplicating any of it here
// would produce two sets of rules that drift.
package webdavfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/webdav"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// FileSystem serves ONE user's tree. A new value is built per request from the
// authenticated session, so there is no way to address another user's files:
// the owner is a field, not a parameter that a handler could forget to pass.
type FileSystem struct {
	svc     *files.Service
	ownerID uuid.UUID
	log     *slog.Logger
}

func New(svc *files.Service, ownerID uuid.UUID, log *slog.Logger) *FileSystem {
	return &FileSystem{svc: svc, ownerID: ownerID, log: log}
}

// clean normalises a WebDAV path to the form the node store uses: absolute, no
// trailing slash, "/" for the root.
func clean(name string) string {
	name = path.Clean("/" + strings.TrimSpace(name))
	if name == "." || name == "" {
		return "/"
	}
	return name
}

func split(name string) (parent, base string) {
	name = clean(name)
	if name == "/" {
		return "/", ""
	}
	parent = path.Dir(name)
	return parent, path.Base(name)
}

// translate maps domain errors onto the os errors webdav.Handler understands.
// Without this every failure becomes a 500, and Finder responds to a 500 by
// giving up on the whole mount rather than reporting one bad file.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrUploadNotFound):
		return os.ErrNotExist
	case errors.Is(err, files.ErrNameTaken):
		return os.ErrExist
	case errors.Is(err, files.ErrInvalidName), errors.Is(err, files.ErrNotAFolder),
		errors.Is(err, files.ErrCycle), errors.Is(err, files.ErrRootReserved):
		return os.ErrInvalid
	case errors.Is(err, files.ErrQuota):
		// ENOSPC is what a filesystem client expects when it runs out of room,
		// and it is what makes Finder say "the disk is full" rather than
		// "an unexpected error occurred".
		return fmt.Errorf("%w: %w", errQuota, err)
	}
	return err
}

var errQuota = errors.New("insufficient storage")

// IsQuotaError lets the HTTP layer map this back to 507.
func IsQuotaError(err error) bool { return errors.Is(err, errQuota) }

// ErrNotExist exposes the sentinel the adapter returns for a missing node, so
// the HTTP layer can filter the discovery probes (.DS_Store, desktop.ini) that
// every Finder and Explorer listing generates out of its logs.
func ErrNotExist() error { return os.ErrNotExist }

func (f *FileSystem) node(ctx context.Context, name string) (*files.Node, error) {
	n, err := f.svc.Store().GetByPath(ctx, f.ownerID, clean(name))
	return n, translate(err)
}

// --- webdav.FileSystem ------------------------------------------------------

func (f *FileSystem) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	parentPath, base := split(name)
	if base == "" {
		return os.ErrExist // the root always exists
	}

	parent, err := f.svc.Store().GetByPath(ctx, f.ownerID, parentPath)
	if err != nil {
		return translate(err)
	}
	_, err = f.svc.Store().CreateFolder(ctx, f.ownerID, parent.ID, base)
	return translate(err)
}

func (f *FileSystem) RemoveAll(ctx context.Context, name string) error {
	n, err := f.node(ctx, name)
	if err != nil {
		return err
	}
	// DELETE moves to the trash rather than purging. A WebDAV client deleting
	// the wrong folder is a recoverable accident here, and the trash is
	// reachable from the web UI. Auto-purge eventually reclaims the space.
	_, err = f.svc.Store().Trash(ctx, f.ownerID, n.ID)
	return translate(err)
}

func (f *FileSystem) Rename(ctx context.Context, oldName, newName string) error {
	n, err := f.node(ctx, oldName)
	if err != nil {
		return err
	}
	newParentPath, base := split(newName)
	if base == "" {
		return os.ErrInvalid
	}

	parent, err := f.svc.Store().GetByPath(ctx, f.ownerID, newParentPath)
	if err != nil {
		return translate(err)
	}

	// A move and a rename in one call, which is what MOVE means. Doing it as
	// two operations would leave a visible intermediate state and could fail
	// halfway, stranding the file somewhere the user never put it.
	_, err = f.svc.Store().MoveOrRename(ctx, f.ownerID, n.ID, parent.ID, base)
	return translate(err)
}

func (f *FileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := f.node(ctx, name)
	if err != nil {
		return nil, err
	}
	return &fileInfo{n}, nil
}

func (f *FileSystem) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	writing := flag&(os.O_WRONLY|os.O_RDWR) != 0

	if !writing {
		n, err := f.node(ctx, name)
		if err != nil {
			return nil, err
		}
		if n.IsFolder() {
			// PROPFIND opens the directory to enumerate it; there is no content
			// to stream, only children.
			return &dirHandle{fs: f, node: n, ctx: ctx}, nil
		}
		_, rc, err := f.svc.Open(ctx, f.ownerID, n.ID)
		if err != nil {
			return nil, translate(err)
		}
		return &readHandle{node: n, rc: rc}, nil
	}

	parentPath, base := split(name)
	if base == "" {
		return nil, os.ErrInvalid
	}
	parent, err := f.svc.Store().GetByPath(ctx, f.ownerID, parentPath)
	if err != nil {
		return nil, translate(err)
	}
	if err := files.ValidateName(base); err != nil {
		return nil, translate(err)
	}

	return newWriteHandle(ctx, f, parent.ID, base)
}

// --- read handle ------------------------------------------------------------

type readHandle struct {
	node *files.Node
	rc   io.ReadSeekCloser
}

func (h *readHandle) Read(p []byte) (int, error)                { return h.rc.Read(p) }
func (h *readHandle) Seek(off int64, whence int) (int64, error) { return h.rc.Seek(off, whence) }
func (h *readHandle) Close() error                              { return h.rc.Close() }
func (h *readHandle) Write([]byte) (int, error)                 { return 0, os.ErrPermission }
func (h *readHandle) Stat() (os.FileInfo, error)                { return &fileInfo{h.node}, nil }
func (h *readHandle) Readdir(int) ([]os.FileInfo, error)        { return nil, os.ErrInvalid }

// --- directory handle -------------------------------------------------------

type dirHandle struct {
	fs   *FileSystem
	node *files.Node
	ctx  context.Context

	mu      sync.Mutex
	entries []os.FileInfo
	offset  int
}

func (h *dirHandle) Read([]byte) (int, error)  { return 0, os.ErrInvalid }
func (h *dirHandle) Write([]byte) (int, error) { return 0, os.ErrInvalid }
func (h *dirHandle) Close() error              { return nil }
func (h *dirHandle) Seek(int64, int) (int64, error) {
	return 0, os.ErrInvalid
}
func (h *dirHandle) Stat() (os.FileInfo, error) { return &fileInfo{h.node}, nil }

// Readdir follows os.File semantics: count <= 0 returns everything at once,
// count > 0 pages and reports io.EOF when exhausted. webdav.Handler uses the
// first form, but getting the second wrong would break any client that pages.
func (h *dirHandle) Readdir(count int) ([]os.FileInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.entries == nil {
		children, err := h.fs.svc.Store().ListChildren(h.ctx, h.fs.ownerID, h.node.ID)
		if err != nil {
			return nil, translate(err)
		}
		h.entries = make([]os.FileInfo, 0, len(children))
		for _, c := range children {
			h.entries = append(h.entries, &fileInfo{c})
		}
	}

	if count <= 0 {
		rest := h.entries[h.offset:]
		h.offset = len(h.entries)
		return rest, nil
	}
	if h.offset >= len(h.entries) {
		return nil, io.EOF
	}
	end := min(h.offset+count, len(h.entries))
	rest := h.entries[h.offset:end]
	h.offset = end
	return rest, nil
}

// --- write handle -----------------------------------------------------------

// writeHandle streams a PUT into the blob store's staging area and commits it
// on Close.
//
// Staging rather than an in-memory buffer because WebDAV clients happily PUT
// multi-gigabyte files, and buffering one in RAM would take the server down.
// Committing on Close rather than incrementally is what makes a PUT atomic:
// an interrupted transfer leaves the previous version of the file intact
// instead of a truncated one.
type writeHandle struct {
	fs       *FileSystem
	ctx      context.Context
	parentID uuid.UUID
	name     string

	stager     blob.Stager
	stagingKey string
	hasher     hash.Hash
	written    int64

	closed bool
	mu     sync.Mutex
}

func newWriteHandle(ctx context.Context, f *FileSystem, parentID uuid.UUID, name string) (webdav.File, error) {
	stager, ok := f.svc.Blobs().(blob.Stager)
	if !ok {
		return nil, errors.New("the configured storage backend cannot accept writes over WebDAV")
	}
	key, err := stager.CreatePartial()
	if err != nil {
		return nil, err
	}
	return &writeHandle{
		fs: f, ctx: ctx, parentID: parentID, name: name,
		stager: stager, stagingKey: key, hasher: sha256.New(),
	}, nil
}

func (h *writeHandle) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, os.ErrClosed
	}

	n, err := h.stager.AppendPartial(h.ctx, h.stagingKey, h.written, h.hasher, bytes.NewReader(p))
	h.written += n
	return int(n), err
}

func (h *writeHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true

	blobKey, err := h.stager.CommitPartial(h.stagingKey)
	if err != nil {
		return err
	}

	_, err = h.fs.svc.Store().PutFile(h.ctx, files.PutFileInput{
		OwnerID:  h.fs.ownerID,
		ParentID: h.parentID,
		Name:     h.name,
		BlobKey:  blobKey,
		Size:     h.written,
		SHA256:   h.hasher.Sum(nil),
		MIME:     files.DetectMIME(h.name, ""),
	})
	if err != nil {
		// The bytes are committed but unreferenced. Removing them now is the
		// tidy path; if it fails the GC still reclaims them, so nothing leaks
		// permanently either way.
		if delErr := h.fs.svc.Blobs().Delete(context.WithoutCancel(h.ctx), blobKey); delErr != nil {
			h.fs.log.Warn("could not remove blob after failed WebDAV write",
				"key", blobKey, "error", delErr)
		}
		return translate(err)
	}
	return nil
}

func (h *writeHandle) Read([]byte) (int, error) { return 0, os.ErrPermission }

// Seek reports the current position for a no-op seek and refuses anything else.
// Some clients probe with Seek(0, io.SeekCurrent) before writing; failing that
// would break them, but honouring a real seek would mean the hash no longer
// describes the file.
func (h *writeHandle) Seek(offset int64, whence int) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if offset == 0 && (whence == io.SeekCurrent || (whence == io.SeekStart && h.written == 0)) {
		return h.written, nil
	}
	return 0, os.ErrInvalid
}

func (h *writeHandle) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }

func (h *writeHandle) Stat() (os.FileInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// The file does not exist yet. Reporting the bytes written so far is what
	// a client expects mid-PUT, and is enough for the Content-Length check
	// webdav.Handler performs.
	return &pendingInfo{name: h.name, size: h.written}, nil
}

// --- FileInfo ---------------------------------------------------------------

type fileInfo struct{ n *files.Node }

func (i *fileInfo) Name() string { return i.n.Name }

func (i *fileInfo) Size() int64 {
	if i.n.IsFolder() {
		return 0
	}
	return i.n.Size
}

func (i *fileInfo) Mode() os.FileMode {
	if i.n.IsFolder() {
		return os.ModeDir | 0o755
	}
	return 0o644
}

func (i *fileInfo) ModTime() time.Time { return i.n.UpdatedAt }
func (i *fileInfo) IsDir() bool        { return i.n.IsFolder() }
func (i *fileInfo) Sys() any           { return i.n }

// ETag makes webdav.Handler emit a strong validator. The content hash genuinely
// identifies the bytes, so a client can cache indefinitely and revalidate
// cheaply — and versions are immutable, so it never goes stale wrongly.
func (i *fileInfo) ETag(context.Context) (string, error) {
	if i.n.IsFile() && len(i.n.SHA256) > 0 {
		return fmt.Sprintf(`"%x"`, i.n.SHA256), nil
	}
	// Returning ErrNotImplemented tells webdav.Handler to fall back to its
	// default modtime+size ETag rather than failing the request.
	return "", webdav.ErrNotImplemented
}

type pendingInfo struct {
	name string
	size int64
}

func (i *pendingInfo) Name() string       { return i.name }
func (i *pendingInfo) Size() int64        { return i.size }
func (i *pendingInfo) Mode() os.FileMode  { return 0o644 }
func (i *pendingInfo) ModTime() time.Time { return time.Now() }
func (i *pendingInfo) IsDir() bool        { return false }
func (i *pendingInfo) Sys() any           { return nil }

var (
	_ os.FileInfo = (*fileInfo)(nil)
	_ os.FileInfo = (*pendingInfo)(nil)
	_ fs.FileInfo = (*fileInfo)(nil)
)
