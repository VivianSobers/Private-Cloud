package cas

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/zeebo/blake3"
)

// Open returns a seekable reader over a manifest's reassembled content.
//
// It must be an io.ReadSeeker because http.ServeContent needs one: Range
// requests, If-Range and 206 responses all depend on seeking, and a reader that
// could only stream forward would make video scrubbing re-read the file from
// byte zero on every seek.
func (s *Store) Open(ctx context.Context, manifestID uuid.UUID) (io.ReadSeekCloser, error) {
	m, err := s.GetManifest(ctx, manifestID)
	if err != nil {
		return nil, err
	}
	return &manifestReader{ctx: ctx, store: s, manifest: m}, nil
}

// manifestReader reassembles a file from its chunks on demand.
//
// One chunk is held decompressed at a time. That is the whole memory budget:
// chunks are capped at 64 KiB, so a thousand concurrent downloads cost tens of
// megabytes rather than the size of the files being served.
type manifestReader struct {
	ctx      context.Context
	store    *Store
	manifest *Manifest

	pos int64

	// The window currently in memory, and the file offset it starts at.
	window      []byte
	windowStart int64
	windowValid bool

	// Chunk references from windowStart onward, loaded lazily. Seeking
	// backwards or far forwards discards them.
	refs      []chunkRef
	refsFrom  int64
	refsValid bool

	closed bool
}

func (r *manifestReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errors.New("read after close")
	}
	if r.pos >= r.manifest.TotalSize {
		return 0, io.EOF
	}
	// A cancelled request must stop reassembling rather than finish serving a
	// response nobody will receive.
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	if !r.windowValid || r.pos < r.windowStart || r.pos >= r.windowStart+int64(len(r.window)) {
		if err := r.loadWindowFor(r.pos); err != nil {
			return 0, err
		}
	}

	off := int(r.pos - r.windowStart)
	n := copy(p, r.window[off:])
	r.pos += int64(n)
	return n, nil
}

// loadWindowFor decompresses the chunk covering the given file offset.
func (r *manifestReader) loadWindowFor(offset int64) error {
	// Reuse the loaded reference list when seeking forward within it; reload
	// when seeking backwards or past its end.
	if !r.refsValid || offset < r.refsFrom || len(r.refs) == 0 {
		refs, err := r.store.chunksFrom(r.ctx, r.manifest.ID, offset)
		if err != nil {
			return fmt.Errorf("load manifest chunks: %w", err)
		}
		if len(refs) == 0 {
			return io.EOF
		}
		r.refs, r.refsFrom, r.refsValid = refs, offset, true
	}

	var target *chunkRef
	for i := range r.refs {
		c := &r.refs[i]
		if offset >= c.offset && offset < c.offset+int64(c.size) {
			target = c
			break
		}
	}
	if target == nil {
		// The cached list does not reach this offset; reload from here.
		refs, err := r.store.chunksFrom(r.ctx, r.manifest.ID, offset)
		if err != nil {
			return fmt.Errorf("load manifest chunks: %w", err)
		}
		if len(refs) == 0 {
			return io.EOF
		}
		r.refs, r.refsFrom, r.refsValid = refs, offset, true
		target = &r.refs[0]
	}

	rc, err := r.store.blobs.Open(r.ctx, target.storageKey)
	if err != nil {
		// A missing chunk is data loss affecting every file that shares it,
		// not just this one. Say so precisely enough to act on.
		return fmt.Errorf("chunk %x is missing from storage: %w", target.hash, err)
	}
	defer rc.Close()

	stored, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read chunk %x: %w", target.hash, err)
	}

	plain, err := Decompress(stored, target.compression, target.size)
	if err != nil {
		return err
	}
	if len(plain) != target.size {
		return fmt.Errorf("chunk %x decompressed to %d bytes, expected %d",
			target.hash, len(plain), target.size)
	}

	// Verify the content matches its address. This is nearly free next to the
	// IO, and it is the only thing standing between silent bit-rot and serving
	// corrupt data as though it were correct. ZFS makes rot unlikely;
	// correctness should not depend on the filesystem being good.
	if got := blake3.Sum256(plain); got != target.hash {
		return fmt.Errorf("chunk %x failed verification: content hashes to %x", target.hash, got)
	}

	r.window, r.windowStart, r.windowValid = plain, target.offset, true
	return nil
}

func (r *manifestReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, errors.New("seek after close")
	}

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.manifest.TotalSize + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}

	// Seeking past the end is legal and Read simply returns EOF, matching
	// os.File. ServeContent relies on that when probing for the length.
	r.pos = abs
	return abs, nil
}

func (r *manifestReader) Close() error {
	r.closed = true
	r.window = nil
	r.refs = nil
	return nil
}
