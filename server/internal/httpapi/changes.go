package httpapi

import (
	"net/http"
	"strconv"
)

// handleChanges is the sync client's cursor endpoint: "what changed since seq N?"
//
// It returns the owner's journal entries past `since`, each carrying the node's
// CURRENT live state for an upsert so the client needs no follow-up call. Three
// scalars steer the client:
//
//   - latest:   the head cursor; when the client's since equals it, it is caught up.
//   - reset:    the client's cursor predates the surviving journal (pruned, or the
//     server was restored behind it) — it must re-sync from scratch and adopt
//     latest, because the entries it needs no longer exist.
//   - has_more: this page hit the limit; poll again immediately.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	user := CurrentUser(r.Context())
	ctx := r.Context()

	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, r, http.StatusBadRequest, "invalid_since", "since must be a non-negative integer")
			return
		}
		since = n
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 500)
	if limit < 1 || limit > 1000 {
		limit = 500
	}

	store := s.files.Store()
	latest, err := store.LatestSeq(ctx, user.ID)
	if err != nil {
		s.serverError(w, r, "latest seq", err)
		return
	}
	earliest, err := store.EarliestSeq(ctx, user.ID)
	if err != nil {
		s.serverError(w, r, "earliest seq", err)
		return
	}

	// The next seq the client needs is since+1. If it is behind the head (latest >
	// since) yet that next seq is already gone — the journal's earliest surviving
	// entry is past it, or the journal is empty (earliest 0) after a full prune —
	// then a partial suffix would silently skip the missing changes. Refuse the
	// delta and tell it to re-sync.
	reset := latest > since && (earliest == 0 || earliest > since+1)
	if reset {
		writeJSON(w, r, http.StatusOK, map[string]any{
			"changes": []any{}, "cursor": latest, "latest": latest,
			"reset": true, "has_more": false,
		})
		return
	}

	changes, err := store.ChangesSince(ctx, user.ID, since, limit)
	if err != nil {
		s.serverError(w, r, "changes since", err)
		return
	}

	entries := make([]map[string]any, 0, len(changes))
	cursor := since
	for _, c := range changes {
		cursor = c.Seq
		e := map[string]any{"seq": c.Seq, "kind": c.Kind, "node_id": c.NodeID, "at": c.At}
		if c.Kind == "upsert" {
			// Embed the node's CURRENT live state. If it is no longer live (a later
			// delete is in this same stream), the node is simply absent and that
			// delete corrects the client — the journal is self-healing.
			if node, err := store.GetLive(ctx, user.ID, c.NodeID); err == nil {
				e["node"] = nodeJSON(node)
			}
		}
		entries = append(entries, e)
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"changes":  entries,
		"cursor":   cursor,
		"latest":   latest,
		"reset":    false,
		"has_more": len(changes) == limit,
	})
}
