package billing

import (
	"testing"
	"time"
)

// The period arithmetic, tested on its own because it is the one piece of this
// package that IS arithmetic. Everything else copies numbers; this decides which
// bucket they are copied into, and a period boundary that is off by an hour or a
// day is a mistake nobody finds until somebody reads a bill.

func TestMonthlyPeriodsAreCalendarMonthsInUTC(t *testing.T) {
	for _, tc := range []struct {
		name       string
		at         time.Time
		start, end string
	}{
		{"mid-month", time.Date(2026, 3, 17, 13, 45, 0, 0, time.UTC),
			"2026-03-01T00:00:00Z", "2026-04-01T00:00:00Z"},
		// The case naive interval arithmetic gets wrong: 31 January plus one
		// month is 3 March in Go's AddDate, so a period computed from "now minus
		// a month" would skip February entirely. Computing from the FIRST is
		// exact because the day is always 1.
		{"last day of a 31-day month", time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC),
			"2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"},
		{"february in a leap year", time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC),
			"2028-02-01T00:00:00Z", "2028-03-01T00:00:00Z"},
		{"december rolls the year", time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC),
			"2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PeriodFor(tc.at, PeriodMonthly)
			if got := p.Start.Format(time.RFC3339); got != tc.start {
				t.Errorf("start = %s, want %s", got, tc.start)
			}
			if got := p.End.Format(time.RFC3339); got != tc.end {
				t.Errorf("end = %s, want %s", got, tc.end)
			}
		})
	}
}

// A timestamp in a zone ahead of UTC on the last of the month must not be filed
// under the next one. Every period boundary in this package is UTC precisely so
// that where the server happens to sit cannot decide which month a byte belongs
// to.
func TestPeriodsIgnoreTheCallersTimeZone(t *testing.T) {
	zone := time.FixedZone("UTC+13", 13*3600)
	// 1 April 06:00 in UTC+13 is 31 March 17:00 UTC, and therefore March.
	local := time.Date(2026, 4, 1, 6, 0, 0, 0, zone)

	p := PeriodFor(local, PeriodMonthly)
	if got := p.Start.Format(time.RFC3339); got != "2026-03-01T00:00:00Z" {
		t.Errorf("start = %s, want the March period — the server's zone decided the month", got)
	}
}

// Half-open, so no instant belongs to two periods. Counting one measurement in
// two months is the failure that would be discovered by the totals not adding
// up, months after the fact.
func TestPeriodsAreHalfOpenAndDoNotOverlap(t *testing.T) {
	march := PeriodFor(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), PeriodMonthly)

	if !march.Contains(march.Start) {
		t.Error("a period does not contain its own start")
	}
	if march.Contains(march.End) {
		t.Error("a period contains its end instant; that instant is in two periods at once")
	}
	next := PeriodFor(march.End, PeriodMonthly)
	if !next.Start.Equal(march.End) {
		t.Errorf("the next period starts at %s, leaving a gap after %s", next.Start, march.End)
	}
}

func TestDailyPeriodsAreCalendarDaysInUTC(t *testing.T) {
	p := PeriodFor(time.Date(2026, 3, 17, 23, 59, 59, 0, time.UTC), PeriodDaily)
	if got := p.Start.Format(time.RFC3339); got != "2026-03-17T00:00:00Z" {
		t.Errorf("start = %s", got)
	}
	if got := p.End.Format(time.RFC3339); got != "2026-03-18T00:00:00Z" {
		t.Errorf("end = %s", got)
	}
}

// A typo in the configured grain must stop the worker rather than silently
// metering by day: thirty times the rows, all of them looking like it worked.
func TestParsePeriodKindRefusesAnythingElse(t *testing.T) {
	if k, err := ParsePeriodKind(""); err != nil || k != PeriodMonthly {
		t.Errorf("empty = (%q, %v), want the monthly default", k, err)
	}
	if _, err := ParsePeriodKind("weekly"); err == nil {
		t.Error("an unknown period was accepted; a silent fallback here is thirty times the rows and no error")
	}
}

// Total is the same sum files.Usage.TotalBytes computes, and stays that way. If
// this ever needs updating, the two accountings have parted company.
func TestSnapshotTotalMatchesTheQuotaAccounting(t *testing.T) {
	s := Snapshot{LiveBytes: 100, TrashBytes: 20, VersionBytes: 3}
	if s.Total() != 123 {
		t.Errorf("Total() = %d, want 123 (live + trash + versions, exactly as the quota check sums them)", s.Total())
	}
}
