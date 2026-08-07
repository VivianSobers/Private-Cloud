package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/engine"
)

// Client talks to a running daemon over its control socket. It is what the
// `pcsync` control subcommands (and later a tray shell) use to read status and
// send pause/resume/sync.
type Client struct {
	http *http.Client
}

// NewClient dials the daemon's control socket at sockPath. Every request goes
// over that socket regardless of the URL host, which is a placeholder.
func NewClient(sockPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
				},
			},
		},
	}
}

// Status fetches the daemon's current status.
func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	var out StatusResponse
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out)
	return out, err
}

// Conflicts fetches the recent conflict copies awaiting a decision.
func (c *Client) Conflicts(ctx context.Context) ([]engine.ConflictRecord, error) {
	var out []engine.ConflictRecord
	err := c.do(ctx, http.MethodGet, "/v1/conflicts", nil, &out)
	return out, err
}

// ClearConflicts dismisses the daemon's conflict log after the files have been
// dealt with.
func (c *Client) ClearConflicts(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/conflicts/clear", nil, nil)
}

// Sync asks the daemon to reconcile now.
func (c *Client) Sync(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/sync", nil, nil)
}

// Pause stops the daemon's automatic syncing.
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/pause", nil, nil)
}

// Resume re-enables the daemon's automatic syncing.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/resume", nil, nil)
}

// Excludes fetches the current selective-sync set.
func (c *Client) Excludes(ctx context.Context) ([]string, error) {
	var out ExcludeSet
	err := c.do(ctx, http.MethodGet, "/v1/excludes", nil, &out)
	return out.Excludes, err
}

// SetExcludes replaces the selective-sync set and returns the normalized result.
func (c *Client) SetExcludes(ctx context.Context, prefixes []string) ([]string, error) {
	var out ExcludeSet
	err := c.do(ctx, http.MethodPut, "/v1/excludes", ExcludeSet{Excludes: prefixes}, &out)
	return out.Excludes, err
}

// do issues one request, sending body as JSON when non-nil and decoding the
// response into out when non-nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	// The host is ignored — the transport always dials the socket — but a syntactically
	// valid URL is still required.
	req, err := http.NewRequestWithContext(ctx, method, "http://pcsync"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control request: %w (is the pcsync daemon running?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("control %s %s: %s: %s", method, path, resp.Status, body)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
