// Package api is the sync client's HTTP interface to the server: authentication,
// the change journal, and the delta protocol. It speaks exactly the endpoints
// server/internal/httpapi exposes, and it verifies every chunk it downloads
// against its address — the client trusts the server's bytes no more than the
// server trusts the client's.
package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/zeebo/blake3"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/chunk"
)

// Client talks to one server as one account.
type Client struct {
	base        string
	username    string
	appPassword string
	userAgent   string
	http        *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// New builds a client. The app password is exchanged for a device token lazily,
// on the first authenticated call, and refreshed when it nears expiry.
func New(base, username, appPassword, userAgent string) *Client {
	if userAgent == "" {
		userAgent = "pcsync"
	}
	return &Client{
		base:        base,
		username:    username,
		appPassword: appPassword,
		userAgent:   userAgent,
		// A generous timeout: a chunk is at most 64 KiB, but a whole-file download
		// or upload of a large blob can legitimately run long on a slow link.
		http: &http.Client{Timeout: 10 * time.Minute},
	}
}

// Error is a non-2xx response, carrying enough to branch on.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("api: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("api: %d", e.Status)
}

// IsUnauthorized reports whether an error is a 401, so a caller can decide to
// re-authenticate.
func IsUnauthorized(err error) bool {
	var e *Error
	return asError(err, &e) && e.Status == http.StatusUnauthorized
}

// IsNotFound reports whether an error is a 404.
func IsNotFound(err error) bool {
	var e *Error
	return asError(err, &e) && e.Status == http.StatusNotFound
}

// IsConflict reports whether an error is a 409 — for this API, always "something
// with that name is already here".
//
// The sync engine needs this to tell a race it WON from a real failure. Creating
// a folder the pull has just created, or uploading to a name that appeared
// between the scan and the write, is the ordinary consequence of two loops
// working on one tree; treating it as fatal aborts a whole sync pass over a
// situation that has already resolved itself correctly.
func IsConflict(err error) bool {
	var e *Error
	return asError(err, &e) && e.Status == http.StatusConflict
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- authentication ---------------------------------------------------------

// authenticate exchanges the app password for a device token.
func (c *Client) authenticate(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/auth/token", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.appPassword)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}

	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode token: %w", err)
	}
	c.mu.Lock()
	c.token, c.expiry = body.Token, body.ExpiresAt
	c.mu.Unlock()
	return nil
}

// bearer returns a valid token, refreshing if it is missing or near expiry.
func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok, exp := c.token, c.expiry
	c.mu.Unlock()
	// Refresh a minute early so a token does not expire mid-request.
	if tok != "" && time.Now().Before(exp.Add(-time.Minute)) {
		return tok, nil
	}
	if err := c.authenticate(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, nil
}

// forgetToken invalidates the cached token so the next call re-authenticates.
func (c *Client) forgetToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// --- request plumbing -------------------------------------------------------

// do issues an authenticated request, retrying once on a 401 after refreshing
// the token — a device token can expire mid-session, and the caller should not
// have to distinguish that from a real auth failure.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	send := func() (*http.Response, error) {
		tok, err := c.bearer(ctx)
		if err != nil {
			return nil, err
		}
		var rdr io.Reader = body
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("User-Agent", c.userAgent)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		return c.http.Do(req)
	}

	resp, err := send()
	if err != nil {
		return nil, err
	}
	// Retry a 401 once — but only when the body is replayable (nil). A streamed
	// body cannot be resent, so those surface the 401 to the caller instead.
	if resp.StatusCode == http.StatusUnauthorized && body == nil {
		resp.Body.Close()
		c.forgetToken()
		return send()
	}
	return resp, nil
}

// doJSON issues a request with a JSON body (or none) and decodes a JSON response
// into out. A non-2xx status becomes an *Error.
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
		ctype = "application/json"
	}
	resp, err := c.do(ctx, method, path, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readError(resp)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func readError(resp *http.Response) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = json.Unmarshal(data, &body)
	return &Error{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
}

// --- node model -------------------------------------------------------------

// Node is the wire shape of a file or folder.
type Node struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	ParentID  string     `json:"parent_id"`
	Size      int64      `json:"size"`
	MIME      string     `json:"mime"`
	Blake3    string     `json:"blake3"`
	SHA256    string     `json:"sha256"`
	TrashedAt *time.Time `json:"trashed_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (n Node) IsFile() bool   { return n.Kind == "file" }
func (n Node) IsFolder() bool { return n.Kind == "folder" }

// ContentHash is the version identity: blake3 for a chunked file, sha256 for a
// whole-file blob. Either uniquely names the content; which algorithm produced it
// does not matter to a client comparing "same or different".
func (n Node) ContentHash() string {
	if n.Blake3 != "" {
		return n.Blake3
	}
	return n.SHA256
}

type nodeEnvelope struct {
	Node Node `json:"node"`
}

// --- browse -----------------------------------------------------------------

// Version reports the server's build version, for a client-vs-server check.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/version", nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

func (c *Client) GetRoot(ctx context.Context) (Node, error) {
	var env nodeEnvelope
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/nodes/root", nil, &env)
	return env.Node, err
}

func (c *Client) ListChildren(ctx context.Context, nodeID string) ([]Node, error) {
	var env struct {
		Children []Node `json:"children"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/nodes/"+nodeID+"/children", nil, &env)
	return env.Children, err
}

// --- mutate -----------------------------------------------------------------

func (c *Client) CreateFolder(ctx context.Context, parentID, name string) (Node, error) {
	var env nodeEnvelope
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/folders",
		map[string]string{"parent_id": parentID, "name": name}, &env)
	return env.Node, err
}

// Trash soft-deletes a node. Matching the API, this is a recoverable delete, not
// a purge — a sync bug that removes the wrong node stays recoverable.
func (c *Client) Trash(ctx context.Context, nodeID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/nodes/"+nodeID, nil, nil)
}

// Move renames and/or reparents a node in one call.
func (c *Client) Move(ctx context.Context, nodeID, name, parentID string) (Node, error) {
	req := map[string]string{}
	if name != "" {
		req["name"] = name
	}
	if parentID != "" {
		req["parent_id"] = parentID
	}
	var env nodeEnvelope
	err := c.doJSON(ctx, http.MethodPatch, "/api/v1/nodes/"+nodeID, req, &env)
	return env.Node, err
}

// Upload sends a whole file as a raw body — the path for blobs below the chunking
// threshold, which the server stores without a manifest.
func (c *Client) Upload(ctx context.Context, parentID, name string, r io.Reader) (Node, error) {
	q := url.Values{"parent_id": {parentID}, "name": {name}}
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/upload?"+q.Encode(), r, "application/octet-stream")
	if err != nil {
		return Node{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return Node{}, readError(resp)
	}
	var env nodeEnvelope
	return env.Node, json.NewDecoder(resp.Body).Decode(&env)
}

// Download opens a file's content for reading. The caller closes the reader.
func (c *Client) Download(ctx context.Context, nodeID string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+nodeID+"/content", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp.Body, nil
}

// --- delta protocol ---------------------------------------------------------

// ManifestChunk is one entry of a chunked file's manifest.
type ManifestChunk struct {
	Hash   string `json:"hash"`
	Offset int64  `json:"offset"`
	Size   int    `json:"size"`
}

// Manifest describes how to fetch a file: chunk by chunk, or whole.
type Manifest struct {
	Kind      string          `json:"kind"` // "chunked" or "whole"
	TotalSize int64           `json:"total_size"`
	Size      int64           `json:"size"`
	Chunks    []ManifestChunk `json:"chunks"`
	Blake3    string          `json:"blake3"`
	SHA256    string          `json:"sha256"`
}

func (c *Client) Manifest(ctx context.Context, nodeID string) (Manifest, error) {
	var m Manifest
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/nodes/"+nodeID+"/manifest", nil, &m)
	return m, err
}

// HaveChunks returns which of the given hashes the server is missing, so the
// client uploads only the new ones.
func (c *Client) HaveChunks(ctx context.Context, hashes []string) ([]string, error) {
	var out struct {
		Missing []string `json:"missing"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/chunks/have",
		map[string][]string{"hashes": hashes}, &out)
	return out.Missing, err
}

// PutChunk uploads one plaintext chunk. The server recomputes its address and
// rejects a mismatch; the client sends plaintext and lets the server compress.
func (c *Client) PutChunk(ctx context.Context, hash string, plain []byte) error {
	resp, err := c.do(ctx, http.MethodPut, "/api/v1/chunks/"+hash,
		bytes.NewReader(plain), "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return readError(resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// GetChunk fetches one chunk and returns its plaintext, decompressed per the
// server's header and VERIFIED against the requested address. A server that
// served the wrong bytes under a hash is caught here, exactly as the server
// catches a client that uploads them.
func (c *Client) GetChunk(ctx context.Context, hash string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/chunks/"+hash, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	stored, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	plainSize, _ := strconv.Atoi(resp.Header.Get("X-Chunk-Plain-Size"))
	plain, err := chunk.Decompress(stored, resp.Header.Get("X-Chunk-Compression"), plainSize)
	if err != nil {
		return nil, err
	}
	want, err := hex.DecodeString(hash)
	if err != nil || len(want) != 32 {
		return nil, fmt.Errorf("api: bad chunk hash %q", hash)
	}
	if sum := blake3.Sum256(plain); !bytes.Equal(sum[:], want) {
		return nil, fmt.Errorf("api: chunk %s failed verification after download", hash)
	}
	return plain, nil
}

// CommitManifest binds already-uploaded chunks into a file version at
// parentID/name. The server owns offsets and total size; the client supplies the
// order and the whole-file hash.
func (c *Client) CommitManifest(ctx context.Context, parentID, name, contentHash string, chunks []string, mime string) (Node, error) {
	q := url.Values{"parent_id": {parentID}, "name": {name}}
	var env nodeEnvelope
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/manifests?"+q.Encode(),
		map[string]any{"content_hash": contentHash, "chunks": chunks, "mime": mime}, &env)
	return env.Node, err
}

// --- change journal ---------------------------------------------------------

// Change is one journal entry. For an upsert, Node carries the node's current
// state; for a delete, only NodeID is meaningful.
type Change struct {
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"` // "upsert" or "delete"
	NodeID string `json:"node_id"`
	Node   *Node  `json:"node"`
}

// Changes is a page of the journal plus the scalars that steer the client.
type Changes struct {
	Changes []Change `json:"changes"`
	Cursor  int64    `json:"cursor"`
	Latest  int64    `json:"latest"`
	Reset   bool     `json:"reset"`
	HasMore bool     `json:"has_more"`
}

// Poll fetches journal entries past `since`. Reset means the cursor predates
// retained history — the caller must re-sync from scratch.
func (c *Client) Poll(ctx context.Context, since int64, limit int) (Changes, error) {
	q := url.Values{"since": {strconv.FormatInt(since, 10)}, "limit": {strconv.Itoa(limit)}}
	var out Changes
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/changes?"+q.Encode(), nil, &out)
	return out, err
}
