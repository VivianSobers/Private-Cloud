package files

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Storage health (Phase 9).
//
// The contract's rule for this endpoint is the important part: read from the
// SAME sources the alerts already use — the zpool textfile collector, restic's
// success timestamp, the jobs table — rather than inventing a second, parallel
// notion of health. Two notions disagree eventually, and then nobody knows which
// one to believe at the moment it matters.
//
// So nothing here runs `zpool status` or `restic snapshots`. The API process
// does not shell out to storage tooling: it reads the files the collectors
// already write, which means the console and the alerts are looking at exactly
// the same numbers.

// PoolHealth is what the zpool textfile collector reported.
type PoolHealth struct {
	Name  string
	State string
	// ScrubAgeSeconds is how long since the last completed scrub, or since pool
	// creation if it has never been scrubbed.
	ScrubAgeSeconds *int64
	// LastScrubClean is nil when the pool has never been scrubbed — distinct from
	// false, which means a scrub ran and found errors.
	LastScrubClean *bool
	// CollectedAt is when the collector last ran. Stale means the collector is
	// not running, which is itself the finding: an old "ONLINE" is not evidence
	// that the pool is online now.
	CollectedAt *time.Time
}

// BackupHealth is what the restic wrapper reported.
type BackupHealth struct {
	LastSuccessAt *time.Time
	LastFailureAt *time.Time
	// AgeSeconds since the last success. Nil when no backup has ever succeeded,
	// which is a much louder condition than a large number.
	AgeSeconds *int64
}

// StorageReport is the admin console's view of the platform's health.
type StorageReport struct {
	Pools  []PoolHealth
	Backup BackupHealth
	// StoredBytes and TrashBytes are what the DATABASE accounts for, across all
	// owners. Deliberately not presented as pool capacity: the application knows
	// what it stored, the collector knows what the disks hold, and conflating the
	// two produces a number that is wrong in both readings.
	StoredBytes int64
	TrashBytes  int64
	FileCount   int64
	// Jobs by state, from the queue itself.
	Jobs map[string]int64
	// CollectorPath is reported so an operator whose numbers are missing can see
	// where the server looked.
	CollectorPath string
	// CollectorAvailable is false when the textfile directory could not be read
	// at all — the common case on a dev box, and worth distinguishing from a pool
	// that is genuinely absent.
	CollectorAvailable bool
}

// GlobalUsage totals stored bytes across every owner.
func (s *Store) GlobalUsage(ctx context.Context) (stored, trash, files int64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT
			coalesce(sum(v.size) FILTER (WHERE n.trashed_at IS NULL), 0),
			coalesce(sum(v.size) FILTER (WHERE n.trashed_at IS NOT NULL), 0),
			count(*)             FILTER (WHERE n.trashed_at IS NULL)
		FROM nodes n
		JOIN file_versions v ON v.id = n.head_version_id
		WHERE n.kind = 'file'`).Scan(&stored, &trash, &files)
	return stored, trash, files, err
}

// ReadCollectorMetrics parses the node_exporter textfile collector output.
//
// A tiny hand-rolled parser rather than a Prometheus client library: this reads
// two files this repository itself writes, in a format it controls, and pulling
// in an exposition parser to read four metric names would be a dependency for
// nothing. Unparseable lines are skipped — a malformed metric must not cost the
// operator the rest of the report.
func ReadCollectorMetrics(dir string) (pools []PoolHealth, backup BackupHealth, available bool) {
	byPool := map[string]*PoolHealth{}

	zpool := parsePromFile(filepath.Join(dir, "privatecloud_zpool.prom"))
	backupFile := parsePromFile(filepath.Join(dir, "privatecloud_backup.prom"))
	if zpool == nil && backupFile == nil {
		return nil, BackupHealth{}, false
	}

	for _, m := range zpool {
		name := m.labels["pool"]
		if name == "" && !strings.HasPrefix(m.name, "privatecloud_zpool_metrics") {
			continue
		}
		p := byPool[name]
		if p == nil && name != "" {
			p = &PoolHealth{Name: name}
			byPool[name] = p
		}

		switch m.name {
		case "privatecloud_zpool_health":
			// The collector emits one series per possible state with a 1 on the
			// current one, so the pool's state is whichever series is set.
			if p != nil && m.value == 1 {
				p.State = m.labels["state"]
			}
		case "privatecloud_zpool_scrub_age_seconds":
			if p != nil {
				age := int64(m.value)
				p.ScrubAgeSeconds = &age
			}
		case "privatecloud_zpool_last_scrub_success":
			if p != nil {
				clean := m.value == 1
				p.LastScrubClean = &clean
			}
		case "privatecloud_zpool_metrics_last_update_timestamp":
			t := time.Unix(int64(m.value), 0).UTC()
			for _, pp := range byPool {
				pp.CollectedAt = &t
			}
		}
	}
	// The collector timestamp may appear before the pools it describes, so it is
	// applied again once every pool exists.
	for _, m := range zpool {
		if m.name == "privatecloud_zpool_metrics_last_update_timestamp" {
			t := time.Unix(int64(m.value), 0).UTC()
			for _, pp := range byPool {
				pp.CollectedAt = &t
			}
		}
	}

	for _, m := range backupFile {
		switch m.name {
		case "privatecloud_backup_last_success_timestamp":
			t := time.Unix(int64(m.value), 0).UTC()
			backup.LastSuccessAt = &t
			age := int64(time.Since(t).Seconds())
			backup.AgeSeconds = &age
		case "privatecloud_backup_last_failure_timestamp":
			if m.value > 0 {
				t := time.Unix(int64(m.value), 0).UTC()
				backup.LastFailureAt = &t
			}
		}
	}

	for _, p := range byPool {
		pools = append(pools, *p)
	}
	return pools, backup, true
}

type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

// parsePromFile reads one textfile-collector file. Returns nil when the file
// cannot be read at all, which is how "no collector here" is distinguished from
// "a collector that reported nothing".
func parsePromFile(path string) []promSample {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	out := []promSample{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// name{labels} value  —  or  name value
		space := strings.LastIndex(line, " ")
		if space < 0 {
			continue
		}
		value, err := strconv.ParseFloat(line[space+1:], 64)
		if err != nil {
			continue
		}
		head := strings.TrimSpace(line[:space])

		s := promSample{value: value, labels: map[string]string{}}
		if open := strings.Index(head, "{"); open >= 0 && strings.HasSuffix(head, "}") {
			s.name = head[:open]
			for _, pair := range splitLabels(head[open+1 : len(head)-1]) {
				k, v, ok := strings.Cut(pair, "=")
				if !ok {
					continue
				}
				s.labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
			}
		} else {
			s.name = head
		}
		out = append(out, s)
	}
	return out
}

// splitLabels splits on commas outside quotes, so a label value containing a
// comma does not become two labels.
func splitLabels(s string) []string {
	var (
		out    []string
		cur    strings.Builder
		inQuot bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			inQuot = !inQuot
			cur.WriteRune(r)
		case r == ',' && !inQuot:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
