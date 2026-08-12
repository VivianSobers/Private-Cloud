package engine

import "time"

// This file makes the sync loop observable and steerable from outside its
// goroutine, so a GUI or CLI can report and control it without scraping logs.
// The reconciliation logic itself is untouched: these are the seams a front-end
// drives, not a change to how sync decides anything.

// Phase is the daemon's coarse activity, the one word a tray icon shows.
type Phase string

const (
	PhaseIdle    Phase = "idle"    // up to date, waiting for the next change
	PhaseSyncing Phase = "syncing" // a pull or push is in flight
	PhaseError   Phase = "error"   // the last run failed; still retrying on the timers
)

// maxConflicts bounds the in-memory conflict log. It is a "needs attention"
// hint, not an audit trail — the conflict files themselves are the durable
// record — so the oldest is dropped once the ring is full.
const maxConflicts = 50

// ConflictRecord is one conflict copy the engine set aside, for the "these files
// need your decision" list.
type ConflictRecord struct {
	Original string    `json:"original"` // server path that took the remote version
	Copy     string    `json:"copy"`     // server path the local edit was moved to
	At       time.Time `json:"at"`
}

// Status is a consistent snapshot of the daemon's sync state.
type Status struct {
	Phase       Phase            `json:"phase"`
	Paused      bool             `json:"paused"`
	LastSync    time.Time        `json:"last_sync"`     // last successful reconcile; zero if none yet
	LastError   string           `json:"last_error"`    // empty once a run succeeds
	LastErrorAt time.Time        `json:"last_error_at"` // zero if no error since start
	Tracked     int              `json:"tracked"`       // nodes in the local state db
	Conflicts   []ConflictRecord `json:"conflicts"`     // recent, newest last
	Since       time.Time        `json:"since"`         // when this daemon started
	IgnoreRules int              `json:"ignore_rules"`  // active .pcsyncignore rules

	// Session transfer tallies since Since.
	PulledFiles int64 `json:"pulled_files"`
	PushedFiles int64 `json:"pushed_files"`
	PulledBytes int64 `json:"pulled_bytes"`
	PushedBytes int64 `json:"pushed_bytes"`
}

// Snapshot returns the current status, including a fresh tracked-item count from
// the state database. Safe to call from any goroutine.
func (e *Engine) Snapshot() Status {
	e.mu.Lock()
	s := Status{
		Phase:       e.phase,
		Paused:      e.paused.Load(),
		LastSync:    e.lastSync,
		LastError:   e.lastErr,
		LastErrorAt: e.lastErrAt,
		Since:       e.since,
		Conflicts:   append([]ConflictRecord(nil), e.conflicts...),
	}
	e.mu.Unlock()

	s.PulledFiles = e.pulledFiles.Load()
	s.PushedFiles = e.pushedFiles.Load()
	s.PulledBytes = e.pulledBytes.Load()
	s.PushedBytes = e.pushedBytes.Load()
	if m := e.ignore.Load(); m != nil {
		s.IgnoreRules = m.Count()
	}

	// Counted outside the lock: it touches the database, and the sync loop is the
	// only writer, so a count taken here is at worst one reconcile stale — fine for
	// a status line, and not worth holding the status mutex across a query.
	if n, err := e.state.Count(); err == nil {
		s.Tracked = n
	}
	return s
}

// countPull and countPush tally one transferred file for the session stats.
func (e *Engine) countPull(size int64) {
	e.pulledFiles.Add(1)
	e.pulledBytes.Add(size)
}

func (e *Engine) countPush(size int64) {
	e.pushedFiles.Add(1)
	e.pushedBytes.Add(size)
}

// setPhase records the coarse activity. A run that failed leaves PhaseError until
// the next successful run clears it, so a glance at the icon shows a lingering
// problem rather than flickering back to idle between retries.
func (e *Engine) setPhase(p Phase) {
	e.mu.Lock()
	e.phase = p
	e.mu.Unlock()
}

// recordResult folds the outcome of one reconcile into the status: success stamps
// the last-sync time and clears the error and phase; failure records the error
// and leaves the phase in error. It returns err unchanged so callers can wrap it
// around a step without swallowing the result.
func (e *Engine) recordResult(err error) error {
	e.mu.Lock()
	if err != nil {
		e.phase = PhaseError
		e.lastErr = err.Error()
		e.lastErrAt = e.clock()
	} else {
		e.phase = PhaseIdle
		e.lastErr = ""
		e.lastSync = e.clock()
	}
	e.mu.Unlock()
	return err
}

// recordConflict appends to the bounded conflict log, dropping the oldest once
// full. Called by conflictCopy the moment it sets a local edit aside.
func (e *Engine) recordConflict(original, copyPath string) {
	e.mu.Lock()
	e.conflicts = append(e.conflicts, ConflictRecord{Original: original, Copy: copyPath, At: e.clock()})
	if len(e.conflicts) > maxConflicts {
		e.conflicts = e.conflicts[len(e.conflicts)-maxConflicts:]
	}
	e.mu.Unlock()
}

// ClearConflicts empties the conflict log, so a person who has dealt with the
// conflict files can dismiss them and stop the status surface nagging. It touches
// only the in-memory hint — the conflict copies on disk (the durable record) are
// untouched, and any new conflict re-populates the log.
func (e *Engine) ClearConflicts() {
	e.mu.Lock()
	e.conflicts = nil
	e.mu.Unlock()
}

// Pause stops the automatic cadences (poll, rescan, and debounced file-event
// pushes). An in-flight reconcile finishes; only the next automatic trigger is
// suppressed. An explicit SyncNow still runs, so paused never means stuck.
func (e *Engine) Pause() { e.paused.Store(true) }

// Resume re-enables the automatic cadences.
func (e *Engine) Resume() { e.paused.Store(false) }

// Paused reports whether automatic syncing is currently suppressed.
func (e *Engine) Paused() bool { return e.paused.Load() }

// SyncNow requests one immediate reconcile. It never blocks: if a request is
// already pending it is coalesced, because one queued sync subsumes another.
func (e *Engine) SyncNow() {
	select {
	case e.syncNow <- struct{}{}:
	default:
	}
}
