package control

import (
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
	err := c.do(ctx, http.MethodGet, "/v1/status", &out)
	return out, err
}

// Conflicts fetches the recent conflict copies awaiting a decision.
func (c *Client) Conflicts(ctx context.Context) ([]engine.ConflictRecord, error) {
	var out []engine.ConflictRecord
	err := c.do(ctx, http.MethodGet, "/v1/conflicts", &out)
	return out, err
}

// Sync asks the daemon to reconcile now.
func (c *Client) Sync(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/sync", nil)
}

// Pause stops the daemon's automatic syncing.
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/pause", nil)
}

// Resume re-enables the daemon's automatic syncing.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/resume", nil)
}

// do issues one request and decodes a JSON body into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path string, out any) error {
	// The host is ignored — the transport always dials the socket — but a syntactically
	// valid URL is still required.
	req, err := http.NewRequestWithContext(ctx, method, "http://pcsync"+path, nil)
	if err != nil {
		return err
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
