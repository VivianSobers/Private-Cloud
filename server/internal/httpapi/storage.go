package httpapi

import (
	"net/http"
	"os"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs/tiering"
)

// Phase 9: storage and platform health for the admin console.
//
// The contract's rule is the important part, and it is followed literally: read
// from the SAME sources the alerts already use — the zpool textfile collector,
// restic's success timestamp, the jobs table — rather than inventing a second,
// parallel notion of health. Two notions disagree eventually, and then nobody
// knows which to believe at the moment it matters.
//
// Nothing here shells out to `zpool` or `restic`. The API process does not run
// storage tooling; it reads the files the collectors already write, so the
// console and Grafana are looking at exactly the same numbers.

// defaultTextfileDir is where node_exporter's textfile collector writes, and
// where scripts/zpool-metrics.sh and scripts/restic-backup.sh put their output.
const defaultTextfileDir = "/var/lib/node_exporter/textfile"

func (s *Server) handleAdminStorage(w http.ResponseWriter, r *http.Request) {
	dir := os.Getenv("PC_TEXTFILE_DIR")
	if dir == "" {
		dir = defaultTextfileDir
	}

	pools, backup, available := files.ReadCollectorMetrics(dir)

	stored, trash, fileCount, err := s.files.Store().GlobalUsage(r.Context())
	if err != nil {
		s.serverError(w, r, "global usage", err)
		return
	}

	poolsOut := make([]map[string]any, 0, len(pools))
	for _, p := range pools {
		entry := map[string]any{"name": p.Name, "state": p.State}
		if p.ScrubAgeSeconds != nil {
			entry["last_scrub_age_seconds"] = *p.ScrubAgeSeconds
		}
		// Absent when the pool has never been scrubbed — distinct from false,
		// which means a scrub ran and found errors. Collapsing the two would
		// report a brand-new pool as damaged.
		if p.LastScrubClean != nil {
			entry["last_scrub_clean"] = *p.LastScrubClean
		}
		if p.CollectedAt != nil {
			// So a reader can tell a healthy pool from a stale report. An old
			// "ONLINE" is not evidence that the pool is online now.
			entry["collected_at"] = *p.CollectedAt
		}
		poolsOut = append(poolsOut, entry)
	}

	backupOut := map[string]any{}
	if backup.LastSuccessAt != nil {
		backupOut["last_success_at"] = *backup.LastSuccessAt
	}
	if backup.LastFailureAt != nil {
		backupOut["last_failure_at"] = *backup.LastFailureAt
	}
	if backup.AgeSeconds != nil {
		backupOut["age_seconds"] = *backup.AgeSeconds
	}

	jobStats := map[string]any{}
	if stats, err := jobs.NewStore(s.db.Pool).Stats(r.Context()); err == nil {
		for _, state := range []string{jobs.StateQueued, jobs.StateRunning, jobs.StateDone, jobs.StateFailed} {
			jobStats[state] = stats[state]
		}
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		// What the DATABASE accounts for, across every owner. Deliberately not
		// labelled as pool capacity: the application knows what it stored, the
		// collector knows what the disks hold, and conflating them produces a
		// number that is wrong in both readings.
		"accounted": map[string]any{
			"stored_bytes": stored,
			"trash_bytes":  trash,
			"file_count":   fileCount,
		},
		"pools":  poolsOut,
		"backup": backupOut,
		"jobs":   jobStats,
		// Tiering, read from the database rather than from the bucket, for the
		// same reason nothing here shells out to zpool: this is what the
		// application believes it put there, and the difference between that and
		// what the bucket actually holds is precisely the discrepancy fsck
		// exists to find. A console that asked the bucket would be a second
		// notion of the truth, and two notions disagree eventually.
		"tiering": s.tieringReport(r),
		"collector": map[string]any{
			// Reported so an operator whose numbers are missing can see where the
			// server looked, rather than guessing.
			"path":      dir,
			"available": available,
		},
	})
}

// tieringReport describes the cold tier, or says plainly that there is none.
//
// The distinction the old hardcoded `false` was protecting is kept exactly:
// "disabled" is not a cold tier holding zero bytes. A deployment with no bucket
// reports enabled:false and a note saying so, rather than a set of zeroes that
// would imply the feature is on and merely empty — a more misleading answer
// than saying nothing at all.
//
// When a tier IS configured, the numbers come from the same rows the demotion
// job writes and fsck reads, so the console, `cloudctl tier status` and the
// checker cannot disagree about how much is cold.
func (s *Server) tieringReport(r *http.Request) map[string]any {
	if s.blobs == nil || !s.blobs.Enabled() {
		return map[string]any{
			"enabled": false,
			"note":    "no cold tier is configured; all content is on the local pool",
		}
	}
	out := map[string]any{"enabled": true}
	if s.db == nil {
		return out
	}
	usage, err := tiering.NewStore(s.db.Pool).Usage(r.Context())
	if err != nil {
		// One failed query costs the cold numbers, not the whole report — the
		// pools, the backup and the job queue are what an operator came for.
		out["note"] = "the cold tier is configured, but its accounting could not be read"
		return out
	}
	out["cold_bytes"] = usage.Bytes()
	out["cold_objects"] = usage.Objects()
	out["cold_blobs"] = usage.ColdBlobs
	out["cold_chunks"] = usage.ColdChunks
	return out
}
