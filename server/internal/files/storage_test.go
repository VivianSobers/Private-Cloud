package files_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Parsing the textfile collectors this repository's own scripts write.
//
// The samples below are copied from the shape scripts/zpool-metrics.sh and
// scripts/restic-backup.sh actually emit, including the HELP/TYPE comments and
// the one-series-per-state encoding of pool health — a parser tested against a
// tidied-up version of the format is a parser tested against the wrong thing.

func writeCollector(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestReadCollectorMetrics(t *testing.T) {
	dir := t.TempDir()

	writeCollector(t, dir, "privatecloud_zpool.prom", `# HELP privatecloud_zpool_health ZFS pool health; value 1 marks the pool's current state.
# TYPE privatecloud_zpool_health gauge
privatecloud_zpool_health{pool="tank",state="ONLINE"} 1
privatecloud_zpool_health{pool="tank",state="DEGRADED"} 0
privatecloud_zpool_health{pool="tank",state="FAULTED"} 0
# HELP privatecloud_zpool_scrub_age_seconds Seconds since the last completed scrub.
# TYPE privatecloud_zpool_scrub_age_seconds gauge
privatecloud_zpool_scrub_age_seconds{pool="tank"} 86400
# TYPE privatecloud_zpool_last_scrub_success gauge
privatecloud_zpool_last_scrub_success{pool="tank"} 1
# TYPE privatecloud_zpool_metrics_last_update_timestamp gauge
privatecloud_zpool_metrics_last_update_timestamp 1770000000
`)
	writeCollector(t, dir, "privatecloud_backup.prom", `# TYPE privatecloud_backup_last_success_timestamp gauge
privatecloud_backup_last_success_timestamp 1769990000
privatecloud_backup_last_failure_timestamp 0
`)

	pools, backup, available := files.ReadCollectorMetrics(dir)
	if !available {
		t.Fatal("collector reported unavailable with both files present")
	}
	if len(pools) != 1 {
		t.Fatalf("got %d pool(s), want 1", len(pools))
	}

	p := pools[0]
	if p.Name != "tank" {
		t.Errorf("name = %q, want tank", p.Name)
	}
	// The collector emits a series per possible state; the pool's state is
	// whichever one carries the 1.
	if p.State != "ONLINE" {
		t.Errorf("state = %q, want ONLINE — the 1-valued series is the current state", p.State)
	}
	if p.ScrubAgeSeconds == nil || *p.ScrubAgeSeconds != 86400 {
		t.Errorf("scrub age = %v, want 86400", p.ScrubAgeSeconds)
	}
	if p.LastScrubClean == nil || !*p.LastScrubClean {
		t.Errorf("last scrub clean = %v, want true", p.LastScrubClean)
	}
	if p.CollectedAt == nil {
		t.Error("no collector timestamp — a stale report would be indistinguishable from a fresh one")
	}

	if backup.LastSuccessAt == nil {
		t.Fatal("no backup success timestamp")
	}
	// A zero failure timestamp means "never failed", not "failed at the epoch".
	if backup.LastFailureAt != nil {
		t.Errorf("last failure = %v, want absent for a zero timestamp", backup.LastFailureAt)
	}
	if backup.AgeSeconds == nil {
		t.Error("no backup age")
	}
}

// A pool that has never been scrubbed reports no verdict, which must not be
// confused with a scrub that ran and found errors.
func TestNeverScrubbedIsNotTheSameAsFailed(t *testing.T) {
	dir := t.TempDir()
	writeCollector(t, dir, "privatecloud_zpool.prom", `privatecloud_zpool_health{pool="tank",state="ONLINE"} 1
privatecloud_zpool_scrub_age_seconds{pool="tank"} 3600
`)

	pools, _, _ := files.ReadCollectorMetrics(dir)
	if len(pools) != 1 {
		t.Fatalf("got %d pool(s), want 1", len(pools))
	}
	if pools[0].LastScrubClean != nil {
		t.Errorf("last scrub clean = %v, want nil — a never-scrubbed pool is not a damaged one",
			*pools[0].LastScrubClean)
	}
}

// No collector at all is distinguishable from a collector reporting nothing —
// the common case on a dev box, and worth telling an operator apart from a pool
// that has genuinely vanished.
func TestMissingCollectorIsReportedAsUnavailable(t *testing.T) {
	pools, backup, available := files.ReadCollectorMetrics(t.TempDir())
	if available {
		t.Error("an empty directory reported an available collector")
	}
	if len(pools) != 0 || backup.LastSuccessAt != nil {
		t.Error("data appeared from nowhere")
	}
}

// A malformed line must not cost the operator the rest of the report.
func TestMalformedLinesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	writeCollector(t, dir, "privatecloud_zpool.prom", `this is not a metric
privatecloud_zpool_health{pool="tank",state="ONLINE"} not-a-number
privatecloud_zpool_health{pool="tank",state="DEGRADED"} 1
privatecloud_zpool_scrub_age_seconds{pool="tank"} 60
`)

	pools, _, available := files.ReadCollectorMetrics(dir)
	if !available {
		t.Fatal("unavailable despite a readable file")
	}
	if len(pools) != 1 || pools[0].State != "DEGRADED" {
		t.Fatalf("pools = %+v, want tank DEGRADED from the one parseable series", pools)
	}
}

// A label value containing a comma must not become two labels.
func TestLabelValuesMayContainCommas(t *testing.T) {
	dir := t.TempDir()
	writeCollector(t, dir, "privatecloud_zpool.prom",
		`privatecloud_zpool_health{pool="tank,backup",state="ONLINE"} 1`+"\n")

	pools, _, _ := files.ReadCollectorMetrics(dir)
	if len(pools) != 1 || pools[0].Name != "tank,backup" {
		t.Fatalf("pools = %+v, want one pool named tank,backup", pools)
	}
}
