package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
)

// Phase 7: user-to-user sharing.
//
// The compatibility rule from the contract is enforced in one place — includeShared
// below. Without the query parameter every endpoint behaves exactly as it did
// before this phase, which is what keeps an already-shipped client correct.

// includeShared reads the opt-in. Anything other than an explicit true is false,
// so a typo widens nothing.
func includeShared(r *http.Request) bool {
	return r.URL.Query().Get("include_shared") == "true"
}

// writeOwner resolves whose tree and whose quota a write lands in.
//
// For the overwhelmingly common case — the caller's own folder — this is the
// caller. For an editor writing into a folder somebody shared with them it is
// the FOLDER'S OWNER, because a grant never moves or copies anything: a file
// created inside a shared folder belongs to the folder's owner, exactly as if
// they had created it themselves, and the bytes count against their quota.
//
// Charging the editor instead would either let one user spend another's quota,
// or leave a file sitting in one tree while counting against a different
// allowance — and then neither party could explain their own usage number.
//
// Reports its own error and returns false when the caller may not write there.
func (s *Server) writeOwner(w http.ResponseWriter, r *http.Request, nodeID uuid.UUID) (uuid.UUID, bool) {
	owner, err := s.files.Store().WriteOwnerFor(r.Context(), CurrentUser(r.Context()).ID, nodeID)
	if err != nil {
		s.writeFilesError(w, r, "resolve write access", err)
		return uuid.Nil, false
	}
	return owner, true
}

// audit records an authorisation-relevant event.
//
// Deliberately swallows its own error. The audit write must never fail the
// request that earned it: a grant that succeeded and was not logged is a gap in
// the record, while a grant refused because the log was busy is a broken
// feature. A failure is logged where the operator will see it.
//
// Detached from the request context so a client disconnecting the instant after
// a successful mutation cannot cancel the record of it.
func (s *Server) audit(r *http.Request, action, target string, detail map[string]any) {
	user := CurrentUser(r.Context())
	if user == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	err := s.files.Store().AppendAudit(ctx, &user.ID, user.Username, action, target,
		RequestID(r.Context()), detail)
	if err != nil {
		s.log.Warn("audit write failed", "action", action, "error", err,
			"request_id", RequestID(r.Context()))
	}
}

func grantJSON(g *files.Grant) map[string]any {
	out := map[string]any{
		"id":         g.ID,
		"node_id":    g.NodeID,
		"path":       g.Path,
		"owner":      g.Owner,
		"grantee":    g.Grantee,
		"role":       g.Role,
		"created_at": g.CreatedAt,
	}
	// Present only on an entry that came from an ancestor. Its ABSENCE is what
	// marks a grant as revocable at the node being asked about, so a client can
	// tell "remove this" from "managed on /Projects" without a second request.
	if g.InheritedFrom != nil {
		out["inherited_from"] = *g.InheritedFrom
	}
	return out
}

func grantsJSON(gs []*files.Grant) []map[string]any {
	out := make([]map[string]any, 0, len(gs))
	for _, g := range gs {
		out = append(out, grantJSON(g))
	}
	return out
}

// accessJSON renders what a caller may do with a node.
//
// Absent on a node the caller owns — its absence is what means "mine", which
// keeps the common response unchanged in both size and meaning.
func accessJSON(a files.Access) map[string]any {
	return map[string]any{"role": a.Role, "owner": a.Owner, "shared": a.Shared}
}

func (s *Server) writeGrantError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, files.ErrGrantNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "no such grant")
	case errors.Is(err, files.ErrInvalidRole):
		// The domain error already names the acceptable roles, from the same list
		// the validator reads — so this cannot drift out of step with what is
		// actually allowed the way a hand-written copy of it did.
		writeError(w, r, http.StatusBadRequest, "invalid_role", err.Error())
	case errors.Is(err, files.ErrCannotGrantToSelf):
		writeError(w, r, http.StatusBadRequest, "invalid_request", "you already have access to your own files")
	case errors.Is(err, files.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "forbidden", "you do not have permission to do that")
	default:
		s.writeFilesError(w, r, op, err)
	}
}

// handleListGrants returns both directions at once: what I shared out, and what
// was shared with me.
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	granted, received, err := s.files.Store().ListGrants(r.Context(), CurrentUser(r.Context()).ID)
	if err != nil {
		s.serverError(w, r, "list grants", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"granted":  grantsJSON(granted),
		"received": grantsJSON(received),
	})
}

// handleListNodeGrants returns the DIRECT grants on one node.
//
// Direct only: an inherited grant belongs to the ancestor that carries it, and
// listing it here would imply it could be revoked here, which it cannot.
func (s *Server) handleListNodeGrants(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	grants, err := s.files.Store().GrantsForNode(r.Context(), CurrentUser(r.Context()).ID, id)
	if err != nil {
		s.writeGrantError(w, r, "node grants", err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"grants": grantsJSON(grants)})
}

func (s *Server) handleCreateGrant(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a username is required")
		return
	}

	user := CurrentUser(r.Context())
	granteeID, err := s.files.Store().UserIDByUsername(r.Context(), body.Username)
	if err != nil {
		// Deliberately the same 404 an unknown node gives: probing this endpoint
		// must not enumerate which usernames exist on the server.
		writeError(w, r, http.StatusNotFound, "not_found", "no such user")
		return
	}

	grant, err := s.files.Store().CreateGrant(r.Context(), user.ID, id, granteeID, body.Role)
	if err != nil {
		s.writeGrantError(w, r, "create grant", err)
		return
	}

	s.audit(r, "grant.create", grant.Path, map[string]any{
		"grantee": grant.Grantee, "role": grant.Role,
	})
	writeJSON(w, r, http.StatusCreated, map[string]any{"grant": grantJSON(grant)})
}

func (s *Server) handleUpdateGrant(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid grant id")
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	grant, err := s.files.Store().UpdateGrant(r.Context(), CurrentUser(r.Context()).ID, id, body.Role)
	if err != nil {
		s.writeGrantError(w, r, "update grant", err)
		return
	}
	s.audit(r, "grant.update", grant.Path, map[string]any{
		"grantee": grant.Grantee, "role": grant.Role,
	})
	writeJSON(w, r, http.StatusOK, map[string]any{"grant": grantJSON(grant)})
}

// handleDeleteGrant revokes. Either party may: the owner takes access back, the
// grantee declines it.
func (s *Server) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_id", "not a valid grant id")
		return
	}
	if err := s.files.Store().DeleteGrant(r.Context(), CurrentUser(r.Context()).ID, id); err != nil {
		s.writeGrantError(w, r, "delete grant", err)
		return
	}
	s.audit(r, "grant.revoke", id.String(), nil)
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "revoked"})
}

// handleShared lists the roots other people granted to this caller.
//
// Roots only. A grant on a folder covers everything beneath it, and listing
// every covered descendant would turn this view into somebody else's whole tree.
func (s *Server) handleShared(w http.ResponseWriter, r *http.Request) {
	user := CurrentUser(r.Context())
	nodes, grants, err := s.files.Store().SharedRoots(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, "shared roots", err)
		return
	}

	byNode := make(map[uuid.UUID]*files.Grant, len(grants))
	for _, g := range grants {
		byNode[g.NodeID] = g
	}

	out := nodesJSON(nodes)
	for i, n := range nodes {
		if g, ok := byNode[n.ID]; ok {
			out[i]["access"] = accessJSON(files.Access{
				Role: g.Role, Owner: g.Owner, Shared: true,
			})
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"items": out})
}

// attachAccess adds the `access` object to nodes the caller does not own.
//
// Owned nodes are left untouched: absence means "mine". Best effort — a failure
// to resolve access degrades the listing to unannotated rather than failing it,
// and the rows are already filtered by the visibility predicate, so nothing
// unauthorised can be exposed by a missing annotation.
func (s *Server) attachAccess(r *http.Request, userID uuid.UUID, nodes []*files.Node, out []map[string]any) []map[string]any {
	accesses, err := s.files.Store().AccessForNodes(r.Context(), userID, nodes)
	if err != nil {
		s.log.Warn("resolve access for listing", "error", err,
			"request_id", RequestID(r.Context()))
		return out
	}
	for i, n := range nodes {
		if a, ok := accesses[n.ID]; ok {
			out[i]["access"] = accessJSON(a)
		}
	}
	return out
}
