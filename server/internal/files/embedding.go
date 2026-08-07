package files

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
)

// Document embeddings and the semantic search over them. Vectors are stored
// content-addressed (dedup, like doc_text) and searched by an exact cosine scan
// in the application — no pgvector, so this runs on stock Postgres, and at
// personal scale an exact scan is fast and always correct. maxSemanticScan bounds
// the work so a pathological corpus cannot turn one query into a full-table sort.
const maxSemanticScan = 100000

// DocForNode returns a node's content hash and its extracted text, for the embed
// worker. ok is false when the node has no extracted text yet — embedding runs
// after extraction, so that is a "not ready", handled by the job re-running.
func (s *Store) DocForNode(ctx context.Context, ownerID, nodeID uuid.UUID) (contentHash []byte, text string, ok bool, err error) {
	var (
		hash []byte
		txt  *string
	)
	err = s.pool.QueryRow(ctx, `
		SELECT coalesce(b.sha256, m.content_hash), dt.text
		FROM nodes n
		JOIN file_versions v ON v.id = n.head_version_id
		LEFT JOIN blobs b     ON b.id = v.blob_id
		LEFT JOIN manifests m ON m.id = v.manifest_id
		LEFT JOIN doc_text dt ON dt.content_hash = coalesce(b.sha256, m.content_hash)
		WHERE n.id = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL`,
		nodeID, ownerID).Scan(&hash, &txt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, nil // gone or not a file
	}
	if err != nil {
		return nil, "", false, err
	}
	if txt == nil {
		return hash, "", false, nil // no extracted text yet
	}
	return hash, *txt, true, nil
}

// HasEmbedding reports whether a content hash is already embedded in a model's
// space, so the worker skips content it has already processed.
func (s *Store) HasEmbedding(ctx context.Context, contentHash []byte, model string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM doc_embedding WHERE content_hash = $1 AND model = $2)`,
		contentHash, model).Scan(&exists)
	return exists, err
}

// PutEmbeddings replaces a content hash's vectors for one model. Replace, not
// append, so re-embedding never leaves stale chunk vectors behind; idempotent, so
// a job that runs twice ends in the same state.
func (s *Store) PutEmbeddings(ctx context.Context, contentHash []byte, model string, dim int, vectors [][]float32) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx,
		`DELETE FROM doc_embedding WHERE content_hash = $1 AND model = $2`, contentHash, model); err != nil {
		return err
	}

	rows := make([][]any, len(vectors))
	for i, v := range vectors {
		rows[i] = []any{contentHash, i, model, dim, embed.Pack(v)}
	}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"doc_embedding"},
		[]string{"content_hash", "chunk_seq", "model", "dim", "vector"},
		pgx.CopyFromRows(rows)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SemanticSearch ranks the owner's live files by cosine similarity of their
// embeddings to a query vector, best chunk per file. It is the brute-force path:
// scan this model's vectors joined to live owned files, score in Go, sort.
func (s *Store) SemanticSearch(ctx context.Context, ownerID uuid.UUID, query []float32, model string, limit int) ([]*SearchResult, error) {
	limit = ClampSearchLimit(limit)

	// Filter by dimension as well as model. Model identity should already pin the
	// dimension, but a model re-trained to a new width while keeping its name would
	// otherwise leave old-width vectors that mismatch the query and silently score
	// zero. Excluding them in SQL means a mixed store degrades to "fewer results",
	// never to wrong ones.
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+`, de.vector
		`+nodeFrom+`
		JOIN doc_embedding de ON de.content_hash = coalesce(b.sha256, m.content_hash)
			AND de.model = $2 AND de.dim = $3
		WHERE n.owner_id = $1 AND n.parent_id IS NOT NULL AND n.trashed_at IS NULL
		LIMIT $4`,
		ownerID, model, len(query), maxSemanticScan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Best cosine per node, keeping each node's row once.
	best := map[uuid.UUID]*SearchResult{}
	for rows.Next() {
		var (
			n    Node
			size *int64
			mime *string
			sha  []byte
			key  *string
			vec  []byte
		)
		if err := rows.Scan(
			&n.ID, &n.OwnerID, &n.ParentID, &n.Kind, &n.Name, &n.Path,
			&n.HeadVersionID, &n.TrashedAt, &n.TrashedRootID, &n.CreatedAt, &n.UpdatedAt,
			&size, &mime, &sha, &key, &n.ManifestID, &vec,
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

		score := embed.Cosine(query, embed.Unpack(vec))
		if cur, ok := best[n.ID]; ok {
			if score > cur.Score {
				cur.Score = score
			}
			continue
		}
		node := n
		best[node.ID] = &SearchResult{Node: &node, Score: score, Semantic: true}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*SearchResult, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	// Highest similarity first; ties resolve toward the more recently updated file.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Node.UpdatedAt.After(out[j].Node.UpdatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneEmbeddings deletes vectors for content no live version references, bounded.
func (s *Store) PruneEmbeddings(ctx context.Context, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM doc_embedding
		WHERE content_hash IN (
			SELECT de.content_hash FROM doc_embedding de
			WHERE NOT EXISTS (
				SELECT 1 FROM file_versions v
				LEFT JOIN blobs b     ON b.id = v.blob_id
				LEFT JOIN manifests m ON m.id = v.manifest_id
				WHERE coalesce(b.sha256, m.content_hash) = de.content_hash
			)
			LIMIT $1
		)`, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
