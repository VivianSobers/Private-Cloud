package files

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// SearchQuery describes a search over one user's tree.
type SearchQuery struct {
	Text string

	// Kind narrows to "file" or "folder". Empty means both.
	Kind string

	// Under restricts results to a subtree ("/photos"). Empty searches
	// everything the user owns.
	Under string

	// IncludeTrashed is off by default. Finding a file you deleted last month
	// and being unable to tell it is deleted is worse than not finding it.
	IncludeTrashed bool

	Limit  int
	Offset int
}

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 200
	// minQueryLen is 2 because pg_trgm indexes trigrams: a single character
	// cannot use the index and degrades to a full scan of the tree.
	minQueryLen = 2
)

// SearchResult is a node plus why it matched.
type SearchResult struct {
	*Node
	// Score is trigram similarity against the name, 0..1. Used only for
	// ordering; exposing it would invite clients to depend on the exact value.
	Score float64
	// MatchedPath is true when the query hit an ancestor directory rather than
	// the file's own name. Surfacing this stops "why is this in my results?"
	MatchedPath bool
	// MatchedContent is true when the query hit the file's EXTRACTED TEXT — an OCR'd
	// receipt found by a word printed on it — rather than its name or path. Without
	// this, a result whose name shares nothing with the query looks like a bug.
	MatchedContent bool
	// Semantic is true for a result from a vector (meaning) search rather than a
	// lexical one — found by what it is about, not a word it contains. Score then
	// carries the cosine similarity.
	Semantic bool
}

// Search finds nodes by filename fragment.
//
// Ordering is deliberate and matters more than it looks:
//
//  1. Exact folded-name matches first. Someone typing a full filename wants
//     that file, not the forty others containing it as a substring.
//  2. Then prefix matches — "budg" ranking "budget.xlsx" above
//     "old-budget.xlsx" matches how people think about names.
//  3. Then trigram similarity, which is what catches typos and middle-of-name
//     fragments.
//  4. Then most recently updated, so ties resolve toward what is being worked
//     on rather than arbitrarily.
func (s *Store) Search(ctx context.Context, ownerID uuid.UUID, q SearchQuery) ([]*SearchResult, error) {
	text := strings.TrimSpace(q.Text)
	if len([]rune(text)) < minQueryLen {
		return nil, fmt.Errorf("%w: search needs at least %d characters", ErrInvalidName, minQueryLen)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	folded := Fold(text)
	pattern := "%" + likePrefix(folded) + "%"

	// The root is excluded: it has an empty name and matching it would put a
	// nameless entry at the top of every result list.
	//
	// doc_text is joined by content hash so a match against a file's extracted
	// text — the OCR of a scanned receipt — comes through the SAME query as a name
	// or path match, ranked below them because a name hit is the stronger signal.
	sql := `
		SELECT ` + nodeCols + `,
		       similarity(n.name_fold, $2) AS score,
		       (n.name_fold NOT LIKE $3) AS matched_path,
		       (dt.text IS NOT NULL AND dt.text ILIKE $3) AS matched_content
		` + nodeFrom + `
		LEFT JOIN doc_text dt ON dt.content_hash = coalesce(b.sha256, m.content_hash)
		WHERE n.owner_id = $1
		  AND n.parent_id IS NOT NULL
		  AND (n.name_fold LIKE $3 OR lower(n.path) LIKE $3 OR dt.text ILIKE $3)`

	args := []any{ownerID, folded, pattern}

	if !q.IncludeTrashed {
		sql += ` AND n.trashed_at IS NULL`
	}
	if q.Kind == KindFile || q.Kind == KindFolder {
		args = append(args, q.Kind)
		sql += fmt.Sprintf(` AND n.kind = $%d`, len(args))
	}
	if under := strings.TrimSpace(q.Under); under != "" && under != "/" {
		under = strings.TrimRight(under, "/")
		args = append(args, likePrefix(under)+"/%")
		sql += fmt.Sprintf(` AND n.path LIKE $%d`, len(args))
	}

	args = append(args, limit, q.Offset)
	sql += fmt.Sprintf(`
		ORDER BY
			(n.name_fold = $2) DESC,
			(n.name_fold LIKE $2 || '%%') DESC,
			similarity(n.name_fold, $2) DESC,
			n.updated_at DESC
		LIMIT $%d OFFSET $%d`, len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	out := make([]*SearchResult, 0, limit)
	for rows.Next() {
		var (
			n              Node
			size           *int64
			mime           *string
			sha            []byte
			key            *string
			score          *float64
			matchedPath    bool
			matchedContent bool
		)
		if err := rows.Scan(
			&n.ID, &n.OwnerID, &n.ParentID, &n.Kind, &n.Name, &n.Path,
			&n.HeadVersionID, &n.TrashedAt, &n.TrashedRootID, &n.CreatedAt, &n.UpdatedAt,
			&size, &mime, &sha, &key, &n.ManifestID, &score, &matchedPath, &matchedContent,
		); err != nil {
			return nil, err
		}
		if size != nil {
			n.Size = *size
		}
		if mime != nil {
			n.MIME = *mime
		}
		n.ContentHash = sha
		if key != nil {
			n.BlobKey = *key
		}

		r := &SearchResult{Node: &n, MatchedPath: matchedPath, MatchedContent: matchedContent}
		if score != nil {
			r.Score = *score
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
