package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/api"
	"github.com/guru-bharadwaj20/private-cloud/client/internal/chunk"
)

// fakeServer is an in-memory stand-in for the API: nodes, a chunk store, and a
// change journal, enough to exercise the engine's reconciliation without a
// network or a database. It mirrors the server's contract where it matters —
// PutChunk verifies addresses, uploads to an existing name make a new version at
// a stable id, and a folder trash cascades into the journal.
type fakeServer struct {
	mu      sync.Mutex
	seq     int64
	nextID  int
	nodes   map[string]*fakeNode // by id
	chunks  map[string][]byte    // hash -> plaintext
	journal []journalEntry
}

type fakeNode struct {
	id, kind, name, parentID, path string
	blake3, sha256                 string
	size                           int64
	manifest                       []string // chunk hashes, for a chunked file
	whole                          []byte   // content, for a whole-file blob
	trashed                        bool
}

type journalEntry struct {
	seq    int64
	kind   string
	nodeID string
}

func newFake() *fakeServer {
	f := &fakeServer{nodes: map[string]*fakeNode{}, chunks: map[string][]byte{}}
	f.nodes["root"] = &fakeNode{id: "root", kind: "folder", name: "", path: "/"}
	return f
}

func (f *fakeServer) mkID() string {
	f.nextID++
	return fmt.Sprintf("n%d", f.nextID)
}

func (f *fakeServer) record(kind, nodeID string) {
	f.seq++
	f.journal = append(f.journal, journalEntry{seq: f.seq, kind: kind, nodeID: nodeID})
}

func joinPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

func (f *fakeServer) liveByPath(path string) *fakeNode {
	for _, n := range f.nodes {
		if !n.trashed && n.path == path {
			return n
		}
	}
	return nil
}

func (f *fakeServer) liveChild(parentID, name string) *fakeNode {
	for _, n := range f.nodes {
		if !n.trashed && n.parentID == parentID && n.name == name {
			return n
		}
	}
	return nil
}

func (f *fakeServer) toAPI(n *fakeNode) api.Node {
	return api.Node{
		ID: n.id, Kind: n.kind, Name: n.name, Path: n.path, ParentID: n.parentID,
		Size: n.size, Blake3: n.blake3, SHA256: n.sha256, MIME: "application/octet-stream",
	}
}

// --- Server interface -------------------------------------------------------

func (f *fakeServer) GetRoot(context.Context) (api.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.toAPI(f.nodes["root"]), nil
}

func (f *fakeServer) ListChildren(_ context.Context, nodeID string) ([]api.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.Node
	for _, n := range f.nodes {
		if !n.trashed && n.parentID == nodeID {
			out = append(out, f.toAPI(n))
		}
	}
	return out, nil
}

func (f *fakeServer) CreateFolder(_ context.Context, parentID, name string) (api.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := f.nodes[parentID]
	if parent == nil {
		return api.Node{}, &api.Error{Status: 404, Code: "not_found"}
	}
	if existing := f.liveChild(parentID, name); existing != nil {
		return f.toAPI(existing), nil
	}
	n := &fakeNode{id: f.mkID(), kind: "folder", name: name, parentID: parentID, path: joinPath(parent.path, name)}
	f.nodes[n.id] = n
	f.record("upsert", n.id)
	return f.toAPI(n), nil
}

func (f *fakeServer) Upload(_ context.Context, parentID, name string, r io.Reader) (api.Node, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return api.Node{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := f.nodes[parentID]
	if parent == nil {
		return api.Node{}, &api.Error{Status: 404, Code: "not_found"}
	}
	sum := sha256.Sum256(data)
	n := f.liveChild(parentID, name)
	if n == nil {
		n = &fakeNode{id: f.mkID(), kind: "file", name: name, parentID: parentID, path: joinPath(parent.path, name)}
		f.nodes[n.id] = n
	}
	n.whole, n.manifest, n.size = data, nil, int64(len(data))
	n.sha256, n.blake3 = hex.EncodeToString(sum[:]), ""
	f.record("upsert", n.id)
	return f.toAPI(n), nil
}

func (f *fakeServer) Download(_ context.Context, nodeID string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[nodeID]
	if n == nil || n.trashed {
		return nil, &api.Error{Status: 404, Code: "not_found"}
	}
	return io.NopCloser(bytes.NewReader(f.assemble(n))), nil
}

// assemble reconstructs a node's bytes from whole storage or its manifest.
func (f *fakeServer) assemble(n *fakeNode) []byte {
	if n.manifest == nil {
		return n.whole
	}
	var buf []byte
	for _, h := range n.manifest {
		buf = append(buf, f.chunks[h]...)
	}
	return buf
}

func (f *fakeServer) Manifest(_ context.Context, nodeID string) (api.Manifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[nodeID]
	if n == nil || n.trashed {
		return api.Manifest{}, &api.Error{Status: 404, Code: "not_found"}
	}
	if n.manifest == nil {
		return api.Manifest{Kind: "whole", Size: n.size, SHA256: n.sha256}, nil
	}
	var (
		chunks []api.ManifestChunk
		off    int64
	)
	for _, h := range n.manifest {
		sz := len(f.chunks[h])
		chunks = append(chunks, api.ManifestChunk{Hash: h, Offset: off, Size: sz})
		off += int64(sz)
	}
	return api.Manifest{Kind: "chunked", TotalSize: n.size, Chunks: chunks, Blake3: n.blake3}, nil
}

func (f *fakeServer) HaveChunks(_ context.Context, hashes []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var missing []string
	for _, h := range hashes {
		if _, ok := f.chunks[h]; !ok {
			missing = append(missing, h)
		}
	}
	return missing, nil
}

func (f *fakeServer) PutChunk(_ context.Context, hash string, plain []byte) error {
	// The load-bearing check, mirrored from the server: bytes must hash to the
	// address claimed for them.
	sum := blake3.Sum256(plain)
	if hex.EncodeToString(sum[:]) != hash {
		return &api.Error{Status: 400, Code: "hash_mismatch"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunks[hash] = bytes.Clone(plain)
	return nil
}

func (f *fakeServer) GetChunk(_ context.Context, hash string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.chunks[hash]
	if !ok {
		return nil, &api.Error{Status: 404, Code: "not_found"}
	}
	return bytes.Clone(c), nil
}

func (f *fakeServer) CommitManifest(_ context.Context, parentID, name, contentHash string, chunks []string, _ string) (api.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parent := f.nodes[parentID]
	if parent == nil {
		return api.Node{}, &api.Error{Status: 404, Code: "not_found"}
	}
	var size int64
	for _, h := range chunks {
		c, ok := f.chunks[h]
		if !ok {
			return api.Node{}, &api.Error{Status: 400, Code: "missing_chunk"}
		}
		size += int64(len(c))
	}
	n := f.liveChild(parentID, name)
	if n == nil {
		n = &fakeNode{id: f.mkID(), kind: "file", name: name, parentID: parentID, path: joinPath(parent.path, name)}
		f.nodes[n.id] = n
	}
	n.manifest, n.whole, n.size = append([]string(nil), chunks...), nil, size
	n.blake3, n.sha256 = contentHash, ""
	f.record("upsert", n.id)
	return f.toAPI(n), nil
}

func (f *fakeServer) Trash(_ context.Context, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[nodeID]
	if n == nil || n.trashed {
		return &api.Error{Status: 404, Code: "not_found"}
	}
	n.trashed = true
	f.record("delete", n.id)
	// Cascade: descendants by path prefix go too, each its own journal row.
	for _, d := range f.nodes {
		if !d.trashed && strings.HasPrefix(d.path, n.path+"/") {
			d.trashed = true
			f.record("delete", d.id)
		}
	}
	return nil
}

func (f *fakeServer) Move(_ context.Context, nodeID, name, parentID string) (api.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodes[nodeID]
	if n == nil || n.trashed {
		return api.Node{}, &api.Error{Status: 404, Code: "not_found"}
	}
	old := n.path
	if name != "" {
		n.name = name
	}
	if parentID != "" {
		n.parentID = parentID
	}
	n.path = joinPath(f.nodes[n.parentID].path, n.name)
	for _, d := range f.nodes {
		if !d.trashed && strings.HasPrefix(d.path, old+"/") {
			d.path = n.path + strings.TrimPrefix(d.path, old)
			f.record("upsert", d.id)
		}
	}
	f.record("upsert", n.id)
	return f.toAPI(n), nil
}

func (f *fakeServer) Poll(_ context.Context, since int64, limit int) (api.Changes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := api.Changes{Latest: f.seq}
	for _, e := range f.journal {
		if e.seq <= since {
			continue
		}
		if len(out.Changes) >= limit {
			out.HasMore = true
			break
		}
		ch := api.Change{Seq: e.seq, Kind: e.kind, NodeID: e.nodeID}
		if e.kind == "upsert" {
			if n := f.nodes[e.nodeID]; n != nil && !n.trashed {
				node := f.toAPI(n)
				ch.Node = &node
			}
		}
		out.Changes = append(out.Changes, ch)
		out.Cursor = e.seq
	}
	if out.Cursor == 0 {
		out.Cursor = since
	}
	return out, nil
}

// --- seeding helpers for tests ----------------------------------------------

func (f *fakeServer) seedFolder(t *testing.T, path string) {
	t.Helper()
	parentID := f.liveByPath(pathParent(path)).id
	if _, err := f.CreateFolder(context.Background(), parentID, pathBase(path)); err != nil {
		t.Fatalf("seed folder %s: %v", path, err)
	}
}

func (f *fakeServer) seedWhole(t *testing.T, path string, content []byte) {
	t.Helper()
	parentID := f.liveByPath(pathParent(path)).id
	if _, err := f.Upload(context.Background(), parentID, pathBase(path), bytes.NewReader(content)); err != nil {
		t.Fatalf("seed whole %s: %v", path, err)
	}
}

func (f *fakeServer) seedChunked(t *testing.T, path string, content []byte) {
	t.Helper()
	ctx := context.Background()
	var order []string
	whole, err := chunk.Split(ctx, bytes.NewReader(content), func(c *chunk.Chunk) error {
		h := hex.EncodeToString(c.Hash[:])
		order = append(order, h)
		return f.PutChunk(ctx, h, c.Plain)
	})
	if err != nil {
		t.Fatalf("seed chunk split %s: %v", path, err)
	}
	parentID := f.liveByPath(pathParent(path)).id
	if _, err := f.CommitManifest(ctx, parentID, pathBase(path), hex.EncodeToString(whole[:]), order, ""); err != nil {
		t.Fatalf("seed chunked %s: %v", path, err)
	}
}

// content returns a live node's current bytes, for asserting what a push landed.
func (f *fakeServer) content(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.liveByPath(path)
	if n == nil {
		return nil, false
	}
	return f.assemble(n), true
}

func (f *fakeServer) isTrashed(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveByPath(path) == nil
}

func pathParent(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func pathBase(p string) string {
	i := strings.LastIndex(p, "/")
	return p[i+1:]
}
