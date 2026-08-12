package files

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// The audit log (Phase 7).
//
// Records AUTHORISATION-RELEVANT events — grants, role changes, admin actions —
// and not every read. A log that records everything is one nobody reads, and on
// this hardware it would outgrow the files it describes.

// AuditEntry is one recorded event.
type AuditEntry struct {
	ID        int64
	At        time.Time
	ActorID   *uuid.UUID
	Actor     string
	Action    string
	Target    string
	RequestID string
	Detail    map[string]any
}

// AuditFilter narrows a read of the log.
type AuditFilter struct {
	Actor  string
	Action string
	From   *time.Time
	To     *time.Time
	Limit  int
	Offset int
}

// AppendAudit records an event.
//
// Best effort by contract: it returns an error so a caller may log it, but no
// caller should fail a request because the audit write failed. A grant that
// succeeded and was not logged is a gap in the record; a grant refused because
// the log was busy is a broken feature.
//
// actor_name is denormalised alongside actor_id because the id is SET NULL when
// a user is deleted — and an audit entry that can no longer say who did the
// thing has lost the only fact that made it worth keeping.
func (s *Store) AppendAudit(ctx context.Context, actorID *uuid.UUID, actorName, action, target, requestID string, detail map[string]any) error {
	payload := []byte("{}")
	if len(detail) > 0 {
		b, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		payload = b
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_log (actor_id, actor_name, action, target, request_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		actorID, actorName, action, target, requestID, payload)
	return err
}

// ListAudit reads the log newest first.
func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]*AuditEntry, error) {
	limit := ClampSearchLimit(f.Limit)

	rows, err := s.pool.Query(ctx, `
		SELECT id, at, actor_id, actor_name, action, target, request_id, detail
		FROM audit_log
		WHERE ($1 = '' OR actor_name = $1)
		  AND ($2 = '' OR action = $2)
		  AND ($3::timestamptz IS NULL OR at >= $3)
		  AND ($4::timestamptz IS NULL OR at <= $4)
		ORDER BY at DESC, id DESC
		LIMIT $5 OFFSET $6`,
		f.Actor, f.Action, f.From, f.To, limit, clampOffset(f.Offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*AuditEntry, 0, limit)
	for rows.Next() {
		var (
			e   AuditEntry
			raw []byte
		)
		if err := rows.Scan(&e.ID, &e.At, &e.ActorID, &e.Actor, &e.Action,
			&e.Target, &e.RequestID, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			// A malformed detail blob must not hide the entry: the actor, action
			// and timestamp are the parts that matter, and they are already read.
			_ = json.Unmarshal(raw, &e.Detail)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
