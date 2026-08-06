package files

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Change is one entry of the sync journal: a node changed (upsert) or left the
// live tree (delete), at a per-owner monotonic seq the client cursors on.
type Change struct {
	Seq    int64
	NodeID uuid.UUID
	Kind   string
	At     time.Time
}

// LatestSeq is the owner's current head cursor — the seq of their most recent
// change, or 0 if they have never changed anything.
//
// It reads sync_state, not max(seq) of the journal, so it stays correct after
// retention prunes the journal's tail: the counter persists even when the rows
// it counted are gone.
func (s *Store) LatestSeq(ctx context.Context, ownerID uuid.UUID) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx,
		`SELECT change_seq FROM sync_state WHERE owner_id = $1`, ownerID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// EarliestSeq is the smallest seq still in the journal, or 0 if it is empty.
// A client whose cursor sits below this has had changes pruned out from under it
// and must re-sync from scratch.
func (s *Store) EarliestSeq(ctx context.Context, ownerID uuid.UUID) (int64, error) {
	var seq *int64
	if err := s.pool.QueryRow(ctx,
		`SELECT min(seq) FROM changes WHERE owner_id = $1`, ownerID).Scan(&seq); err != nil {
		return 0, err
	}
	if seq == nil {
		return 0, nil
	}
	return *seq, nil
}

// ChangesSince returns the owner's journal entries past `since`, in seq order,
// bounded by limit. Because seq is assigned in commit order, "past N" can never
// skip a change that is still to become visible.
func (s *Store) ChangesSince(ctx context.Context, ownerID uuid.UUID, since int64, limit int) ([]Change, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, node_id, kind, at
		FROM changes
		WHERE owner_id = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3`, ownerID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Seq, &c.NodeID, &c.Kind, &c.At); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneChanges trims the journal's tail by age, bounded per pass. It removes only
// the rows, never the sync_state counter, so LatestSeq stays correct and a client
// whose cursor fell behind the surviving history is told to re-sync (via the
// reset signal the changes endpoint computes) rather than silently missing the
// pruned entries.
func (s *Store) PruneChanges(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM changes
		WHERE ctid IN (
			SELECT ctid FROM changes
			WHERE at < now() - $1::interval
			ORDER BY at
			LIMIT $2
		)`, retention.String(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
