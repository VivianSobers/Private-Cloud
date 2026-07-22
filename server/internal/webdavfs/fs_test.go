package webdavfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/net/webdav"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// These tests cover the parts of the adapter that need no database: path
// normalisation, error translation, the FileInfo shims and the handle
// semantics x/net/webdav relies on. The database-backed paths are exercised
// end to end by the WebDAV tests in internal/httpapi.

func TestClean(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{".", "/"},
		{"a", "/a"},
		{"/a/", "/a"},
		{"/a//b", "/a/b"},
		{"/a/./b", "/a/b"},
		{"/a/b/..", "/a"},
		// A client asking for /.. must land on the root, not above it. Finder
		// and rclone both emit these during discovery.
		{"/..", "/"},
		{"/../../etc/passwd", "/etc/passwd"},
		{"  /a/b  ", "/a/b"},
		{"/Photos/2024/img.jpg", "/Photos/2024/img.jpg"},
	}
	for _, c := range cases {
		if got := clean(c.in); got != c.want {
			t.Errorf("clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplit(t *testing.T) {
	cases := []struct{ in, parent, base string }{
		// The root has no base; every caller treats that as "cannot create".
		{"/", "/", ""},
		{"", "/", ""},
		{"/a", "/", "a"},
		{"/a/b", "/a", "b"},
		{"/a/b/", "/a", "b"},
		{"/a/b/c.txt", "/a/b", "c.txt"},
		{"/..", "/", ""},
	}
	for _, c := range cases {
		parent, base := split(c.in)
		if parent != c.parent || base != c.base {
			t.Errorf("split(%q) = (%q, %q), want (%q, %q)", c.in, parent, base, c.parent, c.base)
		}
	}
}

func TestTranslate(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"not found", files.ErrNotFound, os.ErrNotExist},
		{"upload not found", files.ErrUploadNotFound, os.ErrNotExist},
		{"name taken", files.ErrNameTaken, os.ErrExist},
		{"invalid name", files.ErrInvalidName, os.ErrInvalid},
		{"not a folder", files.ErrNotAFolder, os.ErrInvalid},
		{"cycle", files.ErrCycle, os.ErrInvalid},
		{"root reserved", files.ErrRootReserved, os.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := translate(c.in)
			if c.want == nil {
				if got != nil {
					t.Fatalf("translate(nil) = %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, c.want) {
				t.Fatalf("translate(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// Translation has to survive wrapping, because the store returns
// fmt.Errorf("...: %w", ErrNotFound) rather than the bare sentinel.
func TestTranslateUnwrapsWrappedErrors(t *testing.T) {
	wrapped := fmt.Errorf("get by path %q: %w", "/a/b", files.ErrNotFound)
	if !errors.Is(translate(wrapped), os.ErrNotExist) {
		t.Fatalf("wrapped ErrNotFound did not translate to os.ErrNotExist")
	}
}

// Quota is the one case that must NOT collapse into a generic os error: the
// HTTP layer keys 507 off IsQuotaError, and the original cause is still worth
// keeping for the log line.
func TestTranslateQuota(t *testing.T) {
	got := translate(fmt.Errorf("upload: %w", files.ErrQuota))
	if !IsQuotaError(got) {
		t.Fatalf("IsQuotaError(%v) = false, want true", got)
	}
	if !errors.Is(got, files.ErrQuota) {
		t.Fatalf("translated quota error lost its cause: %v", got)
	}
	for _, other := range []error{os.ErrNotExist, os.ErrExist, os.ErrInvalid} {
		if errors.Is(got, other) {
			t.Fatalf("quota error also matches %v", other)
		}
	}
}

// An unrecognised error must pass through untouched. Turning it into
// os.ErrInvalid would tell the client "bad request" about what is actually a
// server fault, and the 500 is the honest answer.
func TestTranslatePassesUnknownErrorsThrough(t *testing.T) {
	sentinel := errors.New("database exploded")
	got := translate(sentinel)
	if got != sentinel { //nolint:errorlint // identity is the point
		t.Fatalf("translate rewrote an unknown error: %v", got)
	}
	if IsQuotaError(got) {
		t.Fatal("unknown error reported as a quota error")
	}
}

func TestIsQuotaErrorRejectsOthers(t *testing.T) {
	if IsQuotaError(nil) {
		t.Fatal("IsQuotaError(nil) = true")
	}
	if IsQuotaError(translate(files.ErrNotFound)) {
		t.Fatal("IsQuotaError said yes to a missing node")
	}
}

func TestErrNotExistIsTheOSSentinel(t *testing.T) {
	// The HTTP layer filters .DS_Store noise with errors.Is against this.
	if !errors.Is(ErrNotExist(), os.ErrNotExist) {
		t.Fatal("ErrNotExist() is not os.ErrNotExist")
	}
}

// --- FileInfo ---------------------------------------------------------------

func folderNode(name string) *files.Node {
	return &files.Node{
		Kind:      files.KindFolder,
		Name:      name,
		Path:      "/" + name,
		Size:      4096, // folders can carry junk here; Size() must ignore it
		UpdatedAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func fileNode(name string, size int64, sum []byte) *files.Node {
	return &files.Node{
		Kind:        files.KindFile,
		Name:        name,
		Path:        "/" + name,
		Size:        size,
		ContentHash: sum,
		UpdatedAt:   time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC),
	}
}

func TestFileInfoForFile(t *testing.T) {
	n := fileNode("notes.txt", 1234, []byte{0xde, 0xad, 0xbe, 0xef})
	i := &fileInfo{n}

	if i.Name() != "notes.txt" {
		t.Errorf("Name() = %q", i.Name())
	}
	if i.Size() != 1234 {
		t.Errorf("Size() = %d, want 1234", i.Size())
	}
	if i.IsDir() {
		t.Error("IsDir() = true for a file")
	}
	if i.Mode() != 0o644 {
		t.Errorf("Mode() = %v, want 0644", i.Mode())
	}
	if !i.ModTime().Equal(n.UpdatedAt) {
		t.Errorf("ModTime() = %v, want %v", i.ModTime(), n.UpdatedAt)
	}
	// Sys() carries the node through so the HTTP layer can reach fields the
	// os.FileInfo interface does not expose.
	if i.Sys() != any(n) {
		t.Error("Sys() did not return the underlying node")
	}
}

func TestFileInfoForFolder(t *testing.T) {
	i := &fileInfo{folderNode("Photos")}

	if !i.IsDir() {
		t.Error("IsDir() = false for a folder")
	}
	// Reporting a folder's stored size would make Finder show a bogus byte
	// count on every directory in the listing.
	if i.Size() != 0 {
		t.Errorf("Size() = %d, want 0 for a folder", i.Size())
	}
	if i.Mode()&os.ModeDir == 0 {
		t.Errorf("Mode() = %v, missing ModeDir", i.Mode())
	}
	if perm := i.Mode().Perm(); perm != 0o755 {
		t.Errorf("Mode().Perm() = %v, want 0755", perm)
	}
}

func TestFileInfoETag(t *testing.T) {
	i := &fileInfo{fileNode("a.bin", 3, []byte{0x01, 0xab, 0xff})}
	tag, err := i.ETag(context.Background())
	if err != nil {
		t.Fatalf("ETag: %v", err)
	}
	// Must be a quoted strong validator; an unquoted value is not a legal ETag.
	if tag != `"01abff"` {
		t.Fatalf("ETag = %s, want %q", tag, `"01abff"`)
	}
}

// A folder, and a file whose hash is missing, must defer to webdav.Handler's
// modtime+size fallback rather than emitting a wrong or empty validator.
func TestFileInfoETagFallsBack(t *testing.T) {
	cases := map[string]*files.Node{
		"folder":      folderNode("Photos"),
		"no hash":     fileNode("a.bin", 3, nil),
		"empty slice": fileNode("a.bin", 3, []byte{}),
	}
	for name, n := range cases {
		t.Run(name, func(t *testing.T) {
			tag, err := (&fileInfo{n}).ETag(context.Background())
			if !errors.Is(err, webdav.ErrNotImplemented) {
				t.Fatalf("err = %v, want webdav.ErrNotImplemented", err)
			}
			if tag != "" {
				t.Fatalf("tag = %q, want empty alongside the error", tag)
			}
		})
	}
}

func TestPendingInfo(t *testing.T) {
	i := &pendingInfo{name: "upload.iso", size: 900}
	if i.Name() != "upload.iso" || i.Size() != 900 || i.IsDir() || i.Sys() != nil {
		t.Fatalf("pendingInfo = %+v", i)
	}
	if i.Mode() != 0o644 {
		t.Errorf("Mode() = %v, want 0644", i.Mode())
	}
	if i.ModTime().IsZero() {
		t.Error("ModTime() is zero; webdav.Handler formats it into a header")
	}
}

// --- dirHandle --------------------------------------------------------------

// dirHandle with pre-filled entries: the paging arithmetic is the part worth
// pinning, and it does not need the store to reach it.
func dirWithEntries(n int) *dirHandle {
	h := &dirHandle{node: folderNode("Photos")}
	h.entries = make([]os.FileInfo, 0, n)
	for i := range n {
		h.entries = append(h.entries, &fileInfo{fileNode(fmt.Sprintf("f%d", i), int64(i), nil)})
	}
	return h
}

func TestDirHandleReaddirAll(t *testing.T) {
	h := dirWithEntries(3)

	got, err := h.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir(0): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	// os.File semantics: a second call after draining returns nothing, not the
	// list again. webdav.Handler would otherwise loop forever on PROPFIND.
	got, err = h.Readdir(0)
	if err != nil {
		t.Fatalf("second Readdir(0): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("second Readdir(0) returned %d entries, want 0", len(got))
	}
}

func TestDirHandleReaddirPages(t *testing.T) {
	h := dirWithEntries(5)

	var names []string
	for {
		batch, err := h.Readdir(2)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Readdir(2): %v", err)
		}
		if len(batch) == 0 {
			t.Fatal("Readdir(2) returned an empty batch without io.EOF")
		}
		for _, e := range batch {
			names = append(names, e.Name())
		}
	}

	want := []string{"f0", "f1", "f2", "f3", "f4"}
	if len(names) != len(want) {
		t.Fatalf("paged names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("paged names = %v, want %v", names, want)
		}
	}
}

func TestDirHandleReaddirEmpty(t *testing.T) {
	h := dirWithEntries(0)

	// count <= 0 on an empty directory is not an error, it is an empty slice.
	all, err := h.Readdir(0)
	if err != nil || len(all) != 0 {
		t.Fatalf("Readdir(0) = (%v, %v), want (empty, nil)", all, err)
	}
	// count > 0 with nothing left is io.EOF.
	if _, err := h.Readdir(10); !errors.Is(err, io.EOF) {
		t.Fatalf("Readdir(10) on an empty dir = %v, want io.EOF", err)
	}
}

// A directory is not a byte stream. Returning zeroes instead of an error here
// would let a client silently download an empty file for every folder.
func TestDirHandleRejectsIO(t *testing.T) {
	h := dirWithEntries(1)

	if _, err := h.Read(make([]byte, 4)); !errors.Is(err, os.ErrInvalid) {
		t.Errorf("Read = %v, want os.ErrInvalid", err)
	}
	if _, err := h.Write([]byte("x")); !errors.Is(err, os.ErrInvalid) {
		t.Errorf("Write = %v, want os.ErrInvalid", err)
	}
	if _, err := h.Seek(0, io.SeekStart); !errors.Is(err, os.ErrInvalid) {
		t.Errorf("Seek = %v, want os.ErrInvalid", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	info, err := h.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("Stat().IsDir() = false on a directory handle")
	}
}

// --- readHandle -------------------------------------------------------------

// nopReadSeekCloser is the minimum an Open result has to satisfy.
type nopReadSeekCloser struct {
	*bytesReader
	closed bool
}

type bytesReader struct {
	b   []byte
	off int64
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += int64(n)
	return n, nil
}

func (r *bytesReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.off = off
	case io.SeekCurrent:
		r.off += off
	case io.SeekEnd:
		r.off = int64(len(r.b)) + off
	}
	return r.off, nil
}

func (c *nopReadSeekCloser) Close() error { c.closed = true; return nil }

func TestReadHandle(t *testing.T) {
	rc := &nopReadSeekCloser{bytesReader: &bytesReader{b: []byte("hello world")}}
	n := fileNode("hello.txt", 11, []byte{0x01})
	h := &readHandle{node: n, rc: rc}

	buf := make([]byte, 5)
	got, err := h.Read(buf)
	if err != nil || got != 5 || string(buf) != "hello" {
		t.Fatalf("Read = (%d, %q, %v)", got, buf, err)
	}

	// Range requests are the reason this handle is seekable at all.
	if off, err := h.Seek(6, io.SeekStart); err != nil || off != 6 {
		t.Fatalf("Seek = (%d, %v)", off, err)
	}
	buf = make([]byte, 5)
	if _, err := h.Read(buf); err != nil || string(buf) != "world" {
		t.Fatalf("Read after Seek = (%q, %v)", buf, err)
	}

	// A GET handle must refuse writes rather than silently discarding them.
	if _, err := h.Write([]byte("x")); !errors.Is(err, os.ErrPermission) {
		t.Errorf("Write = %v, want os.ErrPermission", err)
	}
	if _, err := h.Readdir(0); !errors.Is(err, os.ErrInvalid) {
		t.Errorf("Readdir = %v, want os.ErrInvalid", err)
	}

	info, err := h.Stat()
	if err != nil || info.Name() != "hello.txt" || info.IsDir() {
		t.Fatalf("Stat = (%+v, %v)", info, err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rc.closed {
		t.Error("Close did not close the underlying reader; the fd leaks")
	}
}

// --- writeHandle ------------------------------------------------------------

// Seek is the subtle one: clients probe with Seek(0, SeekCurrent) before
// writing, so refusing outright breaks them, but honouring a real seek would
// leave the running hash describing bytes that are not what was stored.
func TestWriteHandleSeek(t *testing.T) {
	h := &writeHandle{name: "big.iso"}

	if off, err := h.Seek(0, io.SeekCurrent); err != nil || off != 0 {
		t.Fatalf("Seek(0, SeekCurrent) at offset 0 = (%d, %v)", off, err)
	}
	if off, err := h.Seek(0, io.SeekStart); err != nil || off != 0 {
		t.Fatalf("Seek(0, SeekStart) at offset 0 = (%d, %v)", off, err)
	}

	h.written = 100
	// Still a no-op: it reports where we are.
	if off, err := h.Seek(0, io.SeekCurrent); err != nil || off != 100 {
		t.Fatalf("Seek(0, SeekCurrent) at offset 100 = (%d, %v)", off, err)
	}
	// A rewind after bytes have been written is a real seek and must fail.
	if _, err := h.Seek(0, io.SeekStart); !errors.Is(err, os.ErrInvalid) {
		t.Errorf("Seek(0, SeekStart) after writing = %v, want os.ErrInvalid", err)
	}
	for _, c := range []struct {
		off    int64
		whence int
	}{{10, io.SeekStart}, {5, io.SeekCurrent}, {0, io.SeekEnd}, {-1, io.SeekEnd}} {
		if _, err := h.Seek(c.off, c.whence); !errors.Is(err, os.ErrInvalid) {
			t.Errorf("Seek(%d, %d) = %v, want os.ErrInvalid", c.off, c.whence, err)
		}
	}
}

func TestWriteHandleStatReportsProgress(t *testing.T) {
	h := &writeHandle{name: "big.iso", written: 4096}

	info, err := h.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// webdav.Handler compares this against Content-Length; the node does not
	// exist yet, so the bytes written so far is the only honest answer.
	if info.Name() != "big.iso" || info.Size() != 4096 || info.IsDir() {
		t.Fatalf("Stat = %+v", info)
	}
}

func TestWriteHandleRejectsReadAndReaddir(t *testing.T) {
	h := &writeHandle{name: "big.iso"}
	if _, err := h.Read(make([]byte, 4)); !errors.Is(err, os.ErrPermission) {
		t.Errorf("Read = %v, want os.ErrPermission", err)
	}
	if _, err := h.Readdir(0); !errors.Is(err, os.ErrInvalid) {
		t.Errorf("Readdir = %v, want os.ErrInvalid", err)
	}
}

// Close is idempotent because x/net/webdav closes the handle and the HTTP
// layer's defer closes it again. A second commit would create a duplicate
// version of the file.
func TestWriteHandleWriteAfterCloseFails(t *testing.T) {
	h := &writeHandle{name: "big.iso", closed: true}
	if _, err := h.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Write after Close = %v, want os.ErrClosed", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// --- interface conformance --------------------------------------------------

// If any handle stops satisfying webdav.File the failure would otherwise show
// up as a runtime type assertion inside x/net/webdav.
func TestHandlesSatisfyWebdavFile(t *testing.T) {
	var (
		_ webdav.File = (*readHandle)(nil)
		_ webdav.File = (*dirHandle)(nil)
		_ webdav.File = (*writeHandle)(nil)
	)
	var _ webdav.FileSystem = (*FileSystem)(nil)
}
