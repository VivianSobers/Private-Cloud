// Package blob stores opaque byte streams.
//
// The Store interface is deliberately tiny — Put/Open/Stat/Delete by key. That
// narrowness is the whole point: Phase 2 replaces the filesystem implementation
// with content-defined chunking (FastCDC + BLAKE3 + zstd) without any caller
// changing, and a future multi-node deployment could back it with Garage or
// SeaweedFS behind the same four methods.
package blob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrNotFound = errors.New("blob not found")

// PutResult describes what was written.
type PutResult struct {
	Key    string
	Size   int64
	SHA256 []byte
}

type Store interface {
	// Put streams r to storage, returning the assigned key, byte count and
	// content hash. The hash is computed during the copy so the data is never
	// read twice.
	Put(ctx context.Context, r io.Reader) (*PutResult, error)

	// Open returns a ReadSeekCloser so HTTP Range requests can seek rather
	// than discarding a prefix — essential for video scrubbing.
	Open(ctx context.Context, key string) (io.ReadSeekCloser, error)

	Stat(ctx context.Context, key string) (int64, error)
	Delete(ctx context.Context, key string) error
}

// KeyedPutter is implemented by stores that let the CALLER choose the key.
//
// Content-addressed storage needs this: a chunk's key is its hash, decided
// before the write. Put's random key is right for whole files, where the hash
// is not known until the stream has been consumed, and wrong here.
type KeyedPutter interface {
	// PutKeyed writes r at the given key. It reports whether the key already
	// existed, in which case nothing is written.
	//
	// Idempotent by necessity: dedup means the same chunk arrives repeatedly,
	// and re-writing identical bytes would be pure IO. Since the key IS the
	// content hash, an existing key already holds exactly what would be
	// written.
	PutKeyed(ctx context.Context, key string, r io.Reader) (existed bool, err error)
}

// Stager is the optional half of a store: support for partial, resumable
// writes. Kept separate from Store because a future object-storage backend
// would implement resumability through multipart uploads rather than an
// append-at-offset file, and forcing that shape into the core interface would
// make it the wrong abstraction for both.
type Stager interface {
	// CreatePartial allocates an empty staging object and returns its key.
	CreatePartial() (string, error)

	// AppendPartial truncates to offset, then writes r, updating hasher with
	// the bytes written. Truncating first makes the caller's committed offset
	// the single authority on progress.
	AppendPartial(ctx context.Context, key string, offset int64, hasher hash.Hash, r io.Reader) (int64, error)

	// CommitPartial promotes a completed staging object to a real blob and
	// returns its key.
	CommitPartial(key string) (string, error)

	StatPartial(key string) (int64, error)
	RemovePartial(key string) error
	WalkStaging(fn func(key string, size int64) error) error
}

// FSStore stores one file per blob on a local filesystem, fanned out two levels
// deep. The fan-out keeps directory sizes sane: a single flat directory with a
// million entries is slow to list and unpleasant to inspect by hand, and part
// of the value of this layout is that `ls` still works.
//
//	<root>/ab/cd/abcdef0123...
type FSStore struct {
	root string
}

func NewFSStore(root string) (*FSStore, error) {
	if root == "" {
		return nil, errors.New("blob store root is empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create blob root %s: %w", root, err)
	}

	// Fail at startup if the directory is not writable, rather than on the
	// first upload hours later.
	probe := filepath.Join(root, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return nil, fmt.Errorf("blob root %s is not writable: %w", root, err)
	}
	if err := os.Remove(probe); err != nil {
		return nil, fmt.Errorf("blob root %s cleanup failed: %w", root, err)
	}
	return &FSStore{root: root}, nil
}

func (s *FSStore) Root() string { return s.root }

// pathFor maps a key to its on-disk location, rejecting anything that could
// escape the root. Keys are server-generated, but this is the boundary where a
// path traversal would land if that ever stopped being true.
func (s *FSStore) pathFor(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || strings.ContainsAny(key, `\:`) || filepath.IsAbs(key) {
		return "", fmt.Errorf("invalid blob key %q", key)
	}
	full := filepath.Join(s.root, filepath.FromSlash(key))

	// Belt and braces: confirm the resolved path is still under the root.
	rel, err := filepath.Rel(s.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("blob key %q escapes the store root", key)
	}
	return full, nil
}

func (s *FSStore) Put(ctx context.Context, r io.Reader) (*PutResult, error) {
	// 16 random bytes, not a content hash: the hash is not known until the
	// stream has been read, and buffering an arbitrarily large upload to learn
	// it first is not acceptable. Phase 2's CAS layer addresses content by
	// chunk hash instead.
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate blob key: %w", err)
	}
	name := hex.EncodeToString(raw)
	key := fmt.Sprintf("%s/%s/%s", name[0:2], name[2:4], name)

	full, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return nil, fmt.Errorf("create blob directory: %w", err)
	}

	// Write to a temp file in the same directory, then rename. Rename within a
	// filesystem is atomic, so a crash mid-upload leaves a stray temp file
	// (harmless, swept by fsck) rather than a truncated blob that the database
	// believes is complete.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return nil, fmt.Errorf("create temp blob: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), &ctxReader{ctx: ctx, r: r})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("write blob: %w", err)
	}

	// fsync before rename. Without it, a power cut can leave a renamed file
	// whose contents never reached the platter — the database would then point
	// at a blob full of zeros. ZFS makes this unlikely; correctness should not
	// depend on the filesystem being good.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("sync blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("close blob: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("commit blob: %w", err)
	}
	if err := os.Chmod(full, 0o400); err != nil {
		return nil, fmt.Errorf("seal blob: %w", err)
	}

	return &PutResult{Key: key, Size: written, SHA256: hasher.Sum(nil)}, nil
}

// PutKeyed writes r at a caller-chosen key, skipping the write if it exists.
//
// The temp-file-then-rename dance is the same as Put's and for the same reason:
// a crash mid-write must leave a stray temp file rather than a truncated chunk
// that the database believes is complete. The difference is that a torn chunk
// here would corrupt files belonging to everyone who shares it, not just the
// uploader.
func (s *FSStore) PutKeyed(ctx context.Context, key string, r io.Reader) (bool, error) {
	full, err := s.pathFor(key)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(full); err == nil {
		// The key is the content hash, so what is already there is byte for
		// byte what would be written. Rewriting it would be pure IO.
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return false, fmt.Errorf("create chunk directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return false, fmt.Errorf("create temp chunk: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, &ctxReader{ctx: ctx, r: r}); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return false, fmt.Errorf("write chunk: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return false, fmt.Errorf("sync chunk: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return false, fmt.Errorf("close chunk: %w", err)
	}

	if err := os.Rename(tmpName, full); err != nil {
		os.Remove(tmpName)
		return false, fmt.Errorf("commit chunk: %w", err)
	}
	if err := os.Chmod(full, 0o400); err != nil {
		return false, fmt.Errorf("seal chunk: %w", err)
	}
	return false, nil
}

func (s *FSStore) Open(_ context.Context, key string) (io.ReadSeekCloser, error) {
	full, err := s.pathFor(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *FSStore) Stat(_ context.Context, key string) (int64, error) {
	full, err := s.pathFor(key)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (s *FSStore) Delete(_ context.Context, key string) error {
	full, err := s.pathFor(key)
	if err != nil {
		return err
	}
	// Blobs are written read-only; make it writable so the unlink succeeds on
	// filesystems that care.
	_ = os.Chmod(full, 0o600)

	err = os.Remove(full)
	if errors.Is(err, os.ErrNotExist) {
		// Already gone is the desired end state — deleting twice must not be
		// an error, or GC retries turn into permanent failures.
		return nil
	}
	return err
}

// --- staging (resumable uploads) --------------------------------------------

// stagingDir holds partial files for in-progress resumable uploads.
//
// Inside the blob root, not beside it, so that finishing an upload is a rename
// within one filesystem — which is atomic, and does not copy the bytes a second
// time. The leading dot keeps it out of the two-hex-level fan-out that Walk
// treats as blobs.
const stagingDir = ".staging"

// CreatePartial starts a staging file and returns its key.
func (s *FSStore) CreatePartial() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate staging key: %w", err)
	}
	key := stagingDir + "/" + hex.EncodeToString(raw)

	full, err := s.pathFor(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create staging file: %w", err)
	}
	return key, f.Close()
}

// AppendPartial writes r at the given offset, returning the bytes written and
// the running hash after them.
//
// The file is TRUNCATED to offset first. A crash between writing bytes and
// committing the new offset leaves the file longer than the recorded progress;
// truncating makes the recorded offset the single authority, so a resumed
// upload cannot silently splice a duplicated chunk into the middle of a file.
func (s *FSStore) AppendPartial(ctx context.Context, key string, offset int64, hasher hash.Hash, r io.Reader) (int64, error) {
	full, err := s.pathFor(key)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(full, os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if err := f.Truncate(offset); err != nil {
		return 0, fmt.Errorf("truncate staging file: %w", err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}

	written, copyErr := io.Copy(io.MultiWriter(f, hasher), &ctxReader{ctx: ctx, r: r})

	// fsync even on a failed copy: the caller commits whatever arrived, and a
	// recorded offset whose bytes never reached the platter is exactly the
	// corruption resumable uploads are supposed to avoid.
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("sync staging file: %w", err)
	}
	return written, copyErr
}

// CommitPartial moves a completed staging file into the blob layout.
//
// A rename, not a copy: the bytes are already on the right filesystem, and
// re-reading a multi-gigabyte upload to move it would double the IO cost of
// every large file.
func (s *FSStore) CommitPartial(key string) (string, error) {
	src, err := s.pathFor(key)
	if err != nil {
		return "", err
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate blob key: %w", err)
	}
	name := hex.EncodeToString(raw)
	blobKey := fmt.Sprintf("%s/%s/%s", name[0:2], name[2:4], name)

	dst, err := s.pathFor(blobKey)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("create blob directory: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("commit staged upload: %w", err)
	}
	if err := os.Chmod(dst, 0o400); err != nil {
		return "", fmt.Errorf("seal blob: %w", err)
	}
	return blobKey, nil
}

// StatPartial reports the on-disk size of a staging file.
func (s *FSStore) StatPartial(key string) (int64, error) { return s.Stat(context.Background(), key) }

// RemovePartial discards an abandoned upload.
func (s *FSStore) RemovePartial(key string) error { return s.Delete(context.Background(), key) }

// --- maintenance ------------------------------------------------------------

// Walk visits every stored blob key. Used by fsck to find orphans.
func (s *FSStore) Walk(fn func(key string, size int64) error) error {
	return filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Partial uploads are not blobs, and an fsck that reported them as
			// orphans would delete files users are actively uploading.
			if d.Name() == stagingDir {
				return filepath.SkipDir
			}
			return nil
		}
		// Temp files from interrupted uploads are not blobs.
		if strings.HasPrefix(d.Name(), ".upload-") || strings.HasPrefix(d.Name(), ".write-probe") {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(filepath.ToSlash(rel), info.Size())
	})
}

// SweepTempFiles removes leftovers from uploads interrupted by a crash.
//
// Staging files are deliberately NOT swept here: they belong to upload sessions
// the database still knows about, and deleting them would silently destroy an
// upload the client believes it can resume. Expired sessions are reclaimed by
// the GC, which has the database row to prove they are dead.
func (s *FSStore) SweepTempFiles() (int, error) {
	var removed int
	err := filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == stagingDir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".upload-") {
			if os.Remove(p) == nil {
				removed++
			}
		}
		return nil
	})
	return removed, err
}

// WalkStaging visits every partial-upload file, so the GC can detect staging
// files whose session row has vanished.
func (s *FSStore) WalkStaging(fn func(key string, size int64) error) error {
	dir := filepath.Join(s.root, stagingDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if err := fn(stagingDir+"/"+e.Name(), info.Size()); err != nil {
			return err
		}
	}
	return nil
}

// ctxReader aborts a copy when the request context is cancelled, so a client
// that disconnects mid-upload does not leave the server writing to disk for as
// long as the body would have taken.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	return c.r.Read(p)
}
