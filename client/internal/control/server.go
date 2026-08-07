package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/guru-bharadwaj20/private-cloud/client/internal/engine"
)

// Server exposes the engine over the local control socket.
type Server struct {
	eng  Engine
	info Info
	log  *slog.Logger
	http *http.Server
}

// NewServer builds a control server over the given engine and static info.
func NewServer(eng Engine, info Info, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{eng: eng, info: info, log: log}
	s.http = &http.Server{Handler: s.handler()}
	return s
}

// handler wires the routes. Method-qualified patterns (Go 1.22+) make a GET to a
// POST-only control a 405 rather than a silent success.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/conflicts", s.handleConflicts)
	mux.HandleFunc("POST /v1/sync", s.handleSync)
	mux.HandleFunc("POST /v1/pause", s.handlePause)
	mux.HandleFunc("POST /v1/resume", s.handleResume)
	mux.HandleFunc("GET /v1/excludes", s.handleGetExcludes)
	mux.HandleFunc("PUT /v1/excludes", s.handleSetExcludes)
	return mux
}

// Serve listens on the Unix socket at sockPath and serves until Shutdown. A stale
// socket from a previous run is removed first, and the socket is locked down to
// the owner so no other local user can steer this daemon.
func (s *Server) Serve(sockPath string) error {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return fmt.Errorf("control dir: %w", err)
	}
	// A leftover socket file from a crash would make Listen fail with "address in
	// use"; removing it is safe because a live daemon holds an exclusive flock on
	// its state database, so two daemons cannot race here in the first place.
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale socket: %w", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen control socket: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("lock down control socket: %w", err)
	}
	s.log.Info("control socket listening", "path", sockPath)
	if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown stops serving. The socket file is removed by the listener on close.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// Handler exposes the routes for in-process testing without a socket.
func (s *Server) Handler() http.Handler { return s.http.Handler }

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, StatusResponse{
		Status:  s.eng.Snapshot(),
		Server:  s.info.Server,
		Root:    s.info.Root,
		Version: s.info.Version,
	})
}

func (s *Server) handleConflicts(w http.ResponseWriter, _ *http.Request) {
	// Always an array, never null, so a caller can range over it unconditionally.
	conflicts := s.eng.Snapshot().Conflicts
	if conflicts == nil {
		conflicts = []engine.ConflictRecord{}
	}
	writeJSON(w, conflicts)
}

func (s *Server) handleSync(w http.ResponseWriter, _ *http.Request) {
	s.eng.SyncNow()
	w.WriteHeader(http.StatusAccepted) // requested, not necessarily finished
}

func (s *Server) handlePause(w http.ResponseWriter, _ *http.Request) {
	s.eng.Pause()
	writeJSON(w, s.eng.Snapshot())
}

func (s *Server) handleResume(w http.ResponseWriter, _ *http.Request) {
	s.eng.Resume()
	writeJSON(w, s.eng.Snapshot())
}

func (s *Server) handleGetExcludes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, ExcludeSet{Excludes: nonNil(s.eng.Excludes())})
}

func (s *Server) handleSetExcludes(w http.ResponseWriter, r *http.Request) {
	var req ExcludeSet
	// 64 KiB is a generous ceiling for a folder list and a hard stop on a runaway body.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid excludes body: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.eng.SetExcludes(req.Excludes)
	writeJSON(w, ExcludeSet{Excludes: nonNil(s.eng.Excludes())})
}

// nonNil returns an empty slice for nil, so the JSON is [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
