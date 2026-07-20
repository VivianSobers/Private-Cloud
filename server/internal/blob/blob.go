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

// Walk visits every stored blob key. Used by fsck to find orphans.
func (s *FSStore) Walk(fn func(key string, size int64) error) error {
	return filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
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
func (s *FSStore) SweepTempFiles() (int, error) {
	var removed int
	err := filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
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
