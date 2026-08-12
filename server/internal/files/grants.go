package files

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Grants: user-to-user access (Phase 7).
//
// The model in one sentence: a grant says "this person may read (or write) this
// node and everything under it", and nothing else in the system moves.
//
// Inheritance is derived from the node's materialised path, not stored per
// descendant. That is the whole reason `path` is denormalised on every node: a
// folder share has to cover files that do not exist yet, and expanding a grant
// into a row per descendant would have to be maintained on every create, move,
// rename and restore — four places to get it wrong, each of which silently
// grants or removes access.

// Roles. Three exist; only two are grantable.
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	// RoleOwner is what the node's actual owner has. It is never stored in the
	// grants table — a CHECK constraint forbids it — because a row claiming
	// ownership would be a second, contradictory source of truth about who owns
	// a node.
	RoleOwner = "owner"
)

var (
	// ErrGrantNotFound means no such grant, or it is not the caller's to see.
	ErrGrantNotFound = errors.New("grant not found")
	// ErrInvalidRole is a role outside {viewer, editor}.
	ErrInvalidRole = errors.New("role must be viewer or editor")
	// ErrCannotGrantToSelf rejects granting yourself access to your own tree.
	ErrCannotGrantToSelf = errors.New("cannot grant to yourself")
	// ErrForbidden means the caller can see the node but may not do this to it.
	ErrForbidden = errors.New("not permitted")
)

// ValidRole reports whether a role may be stored in a grant.
func ValidRole(role string) bool { return role == RoleViewer || role == RoleEditor }

// Grant is one access record.
type Grant struct {
	ID        uuid.UUID
	NodeID    uuid.UUID
	Path      string
	OwnerID   uuid.UUID
	Owner     string
	GranteeID uuid.UUID
	Grantee   string
	Role      string
	// InheritedFrom names the ancestor a grant came from when it was not made on
	// the node being asked about, so a UI can explain WHY someone has access
	// rather than showing an unexplained entry.
	InheritedFrom *uuid.UUID
	CreatedAt     time.Time
}

// Access is what a caller may do with one node.
type Access struct {
	// Role is owner, editor or viewer.
	Role string
	// Owner is the username of the node's actual owner.
	Owner string
	// Shared is false when the caller owns the node. Its absence in the API
	// response is what means "mine", which keeps the common case unchanged in
	// both size and meaning.
	Shared bool
}

// CanWrite reports whether this access permits modification.
func (a Access) CanWrite() bool { return a.Role == RoleOwner || a.Role == RoleEditor }

const grantCols = `
	g.id, g.node_id, n.path, g.owner_id, ow.username,
	g.grantee_id, gu.username, g.role, g.created_at`

const grantFrom = `
	FROM grants g
	JOIN nodes n  ON n.id = g.node_id
	JOIN users ow ON ow.id = g.owner_id
	JOIN users gu ON gu.id = g.grantee_id`

func scanGrant(row pgx.Row) (*Grant, error) {
	var g Grant
	err := row.Scan(&g.ID, &g.NodeID, &g.Path, &g.OwnerID, &g.Owner,
		&g.GranteeID, &g.Grantee, &g.Role, &g.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGrantNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func scanGrants(rows pgx.Rows) ([]*Grant, error) {
	defer rows.Close()
	out := make([]*Grant, 0, 8)
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AccessFor resolves what a caller may do with one node.
//
// This is the single authorisation question the whole phase turns on, and every
// shared read goes through it. Owner first — the cheap, overwhelmingly common
// case, and the one that must never depend on the grants table being correct.
// Then the best grant on the node or any ancestor of it, where "best" is editor
// over viewer, because access from two grants is the union of what they allow
// rather than whichever happens to sort first.
//
// Returns ErrNotFound when the node does not exist OR when the caller has no
// access at all. Those are deliberately the same answer: distinguishing them
// tells an unauthorised caller that a node id is real.
func (s *Store) AccessFor(ctx context.Context, userID, nodeID uuid.UUID) (Access, error) {
	var (
		ownerID   uuid.UUID
		ownerName string
		path      string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT n.owner_id, u.username, n.path
		FROM nodes n JOIN users u ON u.id = n.owner_id
		WHERE n.id = $1 AND n.trashed_at IS NULL`, nodeID).Scan(&ownerID, &ownerName, &path)
	if errors.Is(err, pgx.ErrNoRows) {
		return Access{}, ErrNotFound
	}
	if err != nil {
		return Access{}, err
	}
	if ownerID == userID {
		return Access{Role: RoleOwner, Owner: ownerName}, nil
	}

	// The ancestor test is a prefix comparison on the materialised path.
	//
	// starts_with, NOT LIKE. The pattern would have to be built from a column
	// here, so the Go-side likePrefix escaping cannot reach it, and an unescaped
	// column in a LIKE pattern makes every metacharacter in a folder NAME into a
	// wildcard: a grant on "100%_done" would also match "1009Xdone". That is a
	// grant silently covering files nobody shared. starts_with takes a plain
	// string and has no metacharacters at all.
	//
	// The trailing separator is load-bearing too — comparing bare prefixes would
	// make a grant on /project cover /projectX.
	var role string
	err = s.pool.QueryRow(ctx, `
		SELECT g.role
		FROM grants g
		JOIN nodes gn ON gn.id = g.node_id
		WHERE g.grantee_id = $1
		  AND gn.owner_id = $2
		  AND gn.trashed_at IS NULL
		  AND ($3 = gn.path OR starts_with($3, gn.path || '/'))
		ORDER BY (g.role = 'editor') DESC
		LIMIT 1`, userID, ownerID, path).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Access{}, ErrNotFound
	}
	if err != nil {
		return Access{}, err
	}
	return Access{Role: role, Owner: ownerName, Shared: true}, nil
}

// AccessForNodes resolves access for many nodes at once.
//
// A listing that called AccessFor per row would be two queries per node on the
// hot path of the file browser. Nodes the caller owns are absent from the result
// — their absence is what "mine" means.
func (s *Store) AccessForNodes(ctx context.Context, userID uuid.UUID, nodes []*Node) (map[uuid.UUID]Access, error) {
	out := map[uuid.UUID]Access{}
	ids := make([]uuid.UUID, 0, len(nodes))
	for _, n := range nodes {
		if n.OwnerID != userID {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (n.id) n.id, g.role, ow.username
		FROM nodes n
		JOIN users ow ON ow.id = n.owner_id
		JOIN grants g ON g.grantee_id = $1 AND g.owner_id = n.owner_id
		JOIN nodes gn ON gn.id = g.node_id AND gn.trashed_at IS NULL
		WHERE n.id = ANY($2)
		  AND (n.path = gn.path OR starts_with(n.path, gn.path || '/'))
		ORDER BY n.id, (g.role = 'editor') DESC`, userID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id    uuid.UUID
			role  string
			owner string
		)
		if err := rows.Scan(&id, &role, &owner); err != nil {
			return nil, err
		}
		out[id] = Access{Role: role, Owner: owner, Shared: true}
	}
	return out, rows.Err()
}

// SharedRoots returns the nodes other people granted to this caller.
//
// Only the roots: a grant on a folder covers everything beneath it, and listing
// every covered descendant here would turn "shared with me" into somebody else's
// entire tree. The client navigates in from a root with ?include_shared=true.
func (s *Store) SharedRoots(ctx context.Context, userID uuid.UUID) ([]*Node, []*Grant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+`
		`+nodeFrom+`
		JOIN grants g ON g.node_id = n.id
		WHERE g.grantee_id = $1 AND n.trashed_at IS NULL
		ORDER BY n.name`, userID)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, nil, err
	}

	grantRows, err := s.pool.Query(ctx, `
		SELECT `+grantCols+`
		`+grantFrom+`
		WHERE g.grantee_id = $1 AND n.trashed_at IS NULL
		ORDER BY n.name`, userID)
	if err != nil {
		return nil, nil, err
	}
	grants, err := scanGrants(grantRows)
	if err != nil {
		return nil, nil, err
	}
	return nodes, grants, nil
}

// GrantsForNode lists the DIRECT grants on one node.
//
// Direct only, deliberately. An inherited grant belongs to the ancestor that
// carries it, so showing it here would imply it could be revoked here — which it
// cannot. The web client made the same choice independently; this is the server
// half of it.
func (s *Store) GrantsForNode(ctx context.Context, ownerID, nodeID uuid.UUID) ([]*Grant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+grantCols+`
		`+grantFrom+`
		WHERE g.node_id = $1 AND g.owner_id = $2
		ORDER BY gu.username`, nodeID, ownerID)
	if err != nil {
		return nil, err
	}
	return scanGrants(rows)
}

// ListGrants returns both directions: what the caller shared out, and what was
// shared with them.
func (s *Store) ListGrants(ctx context.Context, userID uuid.UUID) (granted, received []*Grant, err error) {
	gr, err := s.pool.Query(ctx, `
		SELECT `+grantCols+`
		`+grantFrom+`
		WHERE g.owner_id = $1
		ORDER BY g.created_at DESC`, userID)
	if err != nil {
		return nil, nil, err
	}
	if granted, err = scanGrants(gr); err != nil {
		return nil, nil, err
	}

	rc, err := s.pool.Query(ctx, `
		SELECT `+grantCols+`
		`+grantFrom+`
		WHERE g.grantee_id = $1
		ORDER BY g.created_at DESC`, userID)
	if err != nil {
		return nil, nil, err
	}
	if received, err = scanGrants(rc); err != nil {
		return nil, nil, err
	}
	return granted, received, nil
}

// CreateGrant shares a node with another user.
//
// Only the node's OWNER may grant. An editor deliberately cannot re-share: the
// alternative is that access spreads transitively beyond what the owner can see
// or revoke, which is the failure mode that makes people stop trusting sharing
// entirely.
func (s *Store) CreateGrant(ctx context.Context, ownerID, nodeID, granteeID uuid.UUID, role string) (*Grant, error) {
	if !ValidRole(role) {
		return nil, ErrInvalidRole
	}
	if ownerID == granteeID {
		return nil, ErrCannotGrantToSelf
	}
	// Ownership is re-derived here rather than trusted from the caller: this is
	// the gate on the whole feature.
	if err := s.assertOwnedLive(ctx, ownerID, nodeID); err != nil {
		return nil, err
	}

	return scanGrant(s.pool.QueryRow(ctx, `
		WITH g AS (
			INSERT INTO grants (node_id, owner_id, grantee_id, role)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (node_id, grantee_id) DO UPDATE SET
				role = excluded.role, updated_at = now()
			RETURNING *
		)
		SELECT `+grantCols+`
		FROM g
		JOIN nodes n  ON n.id = g.node_id
		JOIN users ow ON ow.id = g.owner_id
		JOIN users gu ON gu.id = g.grantee_id`,
		nodeID, ownerID, granteeID, role))
}

// UpdateGrant changes a grant's role. Owner only, for the same reason as
// CreateGrant.
func (s *Store) UpdateGrant(ctx context.Context, ownerID, grantID uuid.UUID, role string) (*Grant, error) {
	if !ValidRole(role) {
		return nil, ErrInvalidRole
	}
	return scanGrant(s.pool.QueryRow(ctx, `
		WITH g AS (
			UPDATE grants SET role = $3, updated_at = now()
			WHERE id = $1 AND owner_id = $2
			RETURNING *
		)
		SELECT `+grantCols+`
		FROM g
		JOIN nodes n  ON n.id = g.node_id
		JOIN users ow ON ow.id = g.owner_id
		JOIN users gu ON gu.id = g.grantee_id`,
		grantID, ownerID, role))
}

// DeleteGrant revokes access.
//
// Either side may revoke: the owner takes it back, and the grantee declines it.
// A grantee who cannot remove somebody else's folder from their own "shared with
// me" has no way to clean up, and the only thing they lose by declining is
// access they were free to ignore anyway.
//
// Effective immediately — AccessFor reads the grants table per request, so there
// is no cached decision to expire.
func (s *Store) DeleteGrant(ctx context.Context, userID, grantID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM grants WHERE id = $1 AND (owner_id = $2 OR grantee_id = $2)`,
		grantID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrGrantNotFound
	}
	return nil
}

// --- widening the existing reads --------------------------------------------
//
// The compatibility hazard this phase carries, from the API contract: every
// query in the system filters owner_id = $me, and "files I can see but do not
// own" widens what those endpoints MEAN. That is additive in shape but a
// semantic change, and it is the one place this phase could break a client that
// assumed everything it saw was its own.
//
// So the widening is opt-in per request. Callers that do not pass
// include_shared get exactly what they got before, byte for byte.

// VisibleNodes is the SQL predicate for "this caller may see node n".
//
// $1 must be the caller's user id in whatever query it is spliced into. It is a
// string constant rather than a built expression because the ONE thing that must
// never vary between the endpoints is what "visible" means: search, children and
// tags all widening slightly differently is how a file becomes readable through
// one endpoint and not another.
const VisibleNodes = `(
	n.owner_id = $1 OR EXISTS (
		SELECT 1 FROM grants g
		JOIN nodes gn ON gn.id = g.node_id
		WHERE g.grantee_id = $1
		  AND gn.owner_id = n.owner_id
		  AND gn.trashed_at IS NULL
		  AND (n.path = gn.path OR starts_with(n.path, gn.path || '/'))
	)
)`

// OwnedNodes is the unwidened predicate — the pre-Phase-7 behaviour, kept as a
// named constant so a query reads the same whichever it uses.
const OwnedNodes = `(n.owner_id = $1)`

// Visibility picks the predicate for a request.
func Visibility(includeShared bool) string {
	if includeShared {
		return VisibleNodes
	}
	return OwnedNodes
}

// GetVisible returns a node the caller may see, with what they may do to it.
//
// Replaces GetLive on any path that has to work for shared content. The access
// check and the fetch are separate queries deliberately: AccessFor is the single
// authorisation question, and duplicating its logic into a join here is how the
// two would eventually disagree.
func (s *Store) GetVisible(ctx context.Context, userID, nodeID uuid.UUID) (*Node, Access, error) {
	acc, err := s.AccessFor(ctx, userID, nodeID)
	if err != nil {
		return nil, Access{}, err
	}
	node, err := scanNode(s.pool.QueryRow(ctx, `SELECT `+nodeCols+nodeFrom+`
		WHERE n.id = $1 AND n.trashed_at IS NULL`, nodeID))
	if err != nil {
		return nil, Access{}, err
	}
	return node, acc, nil
}

// ListChildrenVisible lists a folder's contents for a caller who may not own it.
//
// With includeShared false this is exactly ListChildren — same predicate, same
// order — so the default path is unchanged.
func (s *Store) ListChildrenVisible(ctx context.Context, userID, parentID uuid.UUID, includeShared bool) ([]*Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+nodeFrom+`
		WHERE `+Visibility(includeShared)+`
		  AND n.parent_id = $2
		  AND n.trashed_at IS NULL
		ORDER BY (n.kind = 'file'), n.name_fold`, userID, parentID)
	if err != nil {
		return nil, err
	}
	return scanNodes(rows)
}

// ListTagsVisible counts tags per caller rather than per tag globally.
//
// Two people can both have a "receipts" tag; a shared count would tell each of
// them how many files the other has tagged, which is an existence leak through
// a number.
func (s *Store) ListTagsVisible(ctx context.Context, userID uuid.UUID, includeShared bool) ([]TagCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT nt.tag, count(*)
		FROM node_tags nt
		JOIN nodes n ON n.id = nt.node_id
		WHERE `+Visibility(includeShared)+` AND n.trashed_at IS NULL
		GROUP BY nt.tag
		ORDER BY count(*) DESC, nt.tag`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// UserIDByUsername resolves a username for granting. Case-folded, matching how
// uniqueness is enforced.
func (s *Store) UserIDByUsername(ctx context.Context, username string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username_fold = lower($1) AND disabled_at IS NULL`,
		username).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}
