// Package control is the local control surface a GUI or CLI uses to watch and
// steer the running sync daemon. It is a tiny HTTP API served over a Unix domain
// socket in the state directory — never a TCP port, so it is reachable only by a
// process on this machine that can open the socket file, and gated by that
// file's 0600 permission rather than by anything on the network.
//
// The daemon side (Server) wraps the engine; the front-end side (Client) dials
// the socket. Both speak the same small JSON shapes defined here, so a tray
// shell, this package's CLI, and a future mobile companion all drive the daemon
// through one contract.
package control

import "github.com/guru-bharadwaj20/private-cloud/client/internal/engine"

// SocketName is the control socket's filename inside the state directory.
const SocketName = "control.sock"

// StatusResponse is the daemon's answer to GET /v1/status: the engine's live
// snapshot plus the static facts a UI wants to show alongside it.
type StatusResponse struct {
	engine.Status
	Server  string `json:"server"`  // the account's server URL
	Root    string `json:"root"`    // the synced local folder
	Version string `json:"version"` // daemon build version
}

// Info is the static context the daemon stamps onto every status response.
type Info struct {
	Server  string
	Root    string
	Version string
}

// ExcludeSet is the selective-sync payload for GET/PUT /v1/excludes.
type ExcludeSet struct {
	Excludes []string `json:"excludes"`
}

// Engine is the slice of the sync engine the control surface drives — an
// interface so the server can be tested against a stub with no real syncing.
type Engine interface {
	Snapshot() engine.Status
	Pause()
	Resume()
	SyncNow()
	Excludes() []string
	SetExcludes([]string)
}
