package files_test

// Phase 3, slice 1: the change journal.
//
// These pin the properties the sync client leans on: a write records an upsert
// and a trash records a delete, a cursor returns only what is newer, seqs are
// gap-free even under concurrent writes (the reason for the per-owner counter),
// and pruning the tail leaves the counter — and therefore LatestSeq — intact.

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

func hasChange(changes []files.Change, node uuid.UUID, kind string) bool {
	for _, c := range changes {
		if c.NodeID == node && c.Kind == kind {
			return true
		}
	}
	return false
}

func TestJournalRecordsUpsertAndDelete(t *testing.T) {
	f := newFixture(t)
	base, err := f.store.LatestSeq(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}

	node := f.upload(f.root, "doc.txt", "hi")
	changes, err := f.store.ChangesSince(f.ctx, f.user, base, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !hasChange(changes, node.ID, "upsert") {
		t.Error("uploading a file recorded no upsert")
	}

	if _, err := f.store.Trash(f.ctx, f.user, node.ID); err != nil {
		t.Fatal(err)
	}
	changes, err = f.store.ChangesSince(f.ctx, f.user, base, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !hasChange(changes, node.ID, "delete") {
		t.Error("trashing a file recorded no delete")
	}
}

func TestJournalCursorReturnsOnlyNewer(t *testing.T) {
	f := newFixture(t)
	f.upload(f.root, "one.txt", "a")
	mid, err := f.store.LatestSeq(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	later := f.upload(f.root, "two.txt", "b")

	changes, err := f.store.ChangesSince(f.ctx, f.user, mid, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Everything returned is strictly past the cursor, in order.
	prev := mid
	for _, c := range changes {
		if c.Seq <= mid {
			t.Errorf("cursor %d returned an older seq %d", mid, c.Seq)
		}
		if c.Seq <= prev && c.Seq != changes[0].Seq {
			t.Error("changes are not in ascending seq order")
		}
		prev = c.Seq
	}
	if !hasChange(changes, later.ID, "upsert") {
		t.Error("the newer file is missing from the delta")
	}
}

func TestJournalSeqGapFreeUnderConcurrency(t *testing.T) {
	// The reason the cursor is a per-owner counter, not a bigserial: concurrent
	// writes must produce contiguous, gap-free seqs so a client can never advance
	// past a change that is still to become visible.
	f := newFixture(t)

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.store.CreateFolder(f.ctx, f.user, f.root, uuid.NewString()[:8])
		}()
	}
	wg.Wait()

	changes, err := f.store.ChangesSince(f.ctx, f.user, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	var min, max int64
	for i, c := range changes {
		if seen[c.Seq] {
			t.Fatalf("duplicate seq %d", c.Seq)
		}
		seen[c.Seq] = true
		if i == 0 || c.Seq < min {
			min = c.Seq
		}
		if c.Seq > max {
			max = c.Seq
		}
	}
	// Contiguous from min to max: every seq present exactly once.
	for s := min; s <= max; s++ {
		if !seen[s] {
			t.Errorf("gap in the journal at seq %d", s)
		}
	}
}

func TestPruneChangesKeepsCounter(t *testing.T) {
	f := newFixture(t)
	f.upload(f.root, "keep.txt", "x")
	latest, err := f.store.LatestSeq(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}

	// Backdate this owner's journal, then prune it away.
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE changes SET at = now() - interval '60 days' WHERE owner_id = $1`, f.user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PruneChanges(f.ctx, 30*24*time.Hour, 5000); err != nil {
		t.Fatal(err)
	}

	// The counter — and therefore the head cursor — survives the prune.
	after, err := f.store.LatestSeq(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if after != latest {
		t.Errorf("LatestSeq moved from %d to %d across a prune", latest, after)
	}
	// And the journal's earliest is now above where a caught-up-behind client sat.
	earliest, err := f.store.EarliestSeq(f.ctx, f.user)
	if err != nil {
		t.Fatal(err)
	}
	if earliest != 0 && earliest <= 1 {
		t.Errorf("prune left early entries behind: earliest = %d", earliest)
	}
}
