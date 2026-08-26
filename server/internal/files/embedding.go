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

// Whether this database has the pgvector column, resolved once and cached.
//
// Probed rather than configured, because it is a property of the database the
// process is pointed at, not a decision an operator should have to keep in step
// with it: migration 00026 adds doc_embedding.vec where the extension exists and
// silently does not where it does not, so asking the catalog is asking the only
// thing that actually knows. Cached because the answer cannot change under a
// running process without a migration, and a migration means a restart.
//
// The zero value is "not yet probed", so a Store built in a test that never
// touches semantic search never issues the query.
func (s *Store) pgvectorReady(ctx context.Context) bool {
	s.pgvecOnce.Do(func() {
		var ok bool
		err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'doc_embedding' AND column_name = 'vec'
			)`).Scan(&ok)
		// A probe that errors is a probe that has not proved the fast path is
		// available, and the exact scan is always correct — so failure degrades
		// rather than propagating.
		s.pgvec = err == nil && ok
	})
	return s.pgvec
}

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

	// Write `vec` alongside the packed bytes where the column exists, so the
	// indexed path never lags the source of truth for anything written after
	// migration 00026. CopyFrom cannot cast text to vector, so the pgvector path
	// is an INSERT; it runs at most maxChunks rows per document, which is not
	// the hot path CopyFrom exists for.
	if s.pgvectorReady(ctx) {
		for i, v := range vectors {
			if _, err := tx.Exec(ctx, `
				INSERT INTO doc_embedding (content_hash, chunk_seq, model, dim, vector, vec)
				VALUES ($1, $2, $3, $4, $5, $6::vector)`,
				contentHash, i, model, dim, embed.Pack(v), embed.Literal(v)); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
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

// SemanticSearch ranks the caller's live files by cosine similarity of their
// embeddings to a query vector, best chunk per file. It is the brute-force path:
// scan this model's vectors joined to live visible files, score in Go, sort.
//
// The ACL filter is applied to the NODE rows, never to the vectors, and that is
// not an implementation detail. Embeddings are content-addressed, so two users
// owning the same document share one vector row by construction; filtering the
// vectors would either hide a document from someone entitled to it or — far
// worse — let one user's query surface the existence of another's document
// through a similarity score.
func (s *Store) SemanticSearch(ctx context.Context, ownerID uuid.UUID, query []float32, model string, limit int, includeShared bool) ([]*SearchResult, error) {
	limit = ClampSearchLimit(limit)

	if s.pgvectorReady(ctx) {
		out, ok, err := s.semanticSearchIndexed(ctx, ownerID, query, model, limit, includeShared)
		if err != nil {
			return nil, err
		}
		if ok {
			return out, nil
		}
		// Not ok means this model's vec copy is still incomplete; fall through
		// to the scan, which reads the packed bytes and is never incomplete.
	}

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
		WHERE `+Visibility(includeShared)+` AND n.parent_id IS NOT NULL AND n.trashed_at IS NULL
		-- Ordered so the bound below truncates deterministically. Without it the
		-- planner decides which vectors get scored once a corpus exceeds the cap,
		-- and the same query returns different top results run to run. Newest
		-- first is the useful half to keep when something has to be dropped.
		ORDER BY n.updated_at DESC, n.id, de.chunk_seq
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

// candidateFanout is how many chunk rows the indexed path pulls per result it
// intends to return. A document is several chunks and the best one wins, so the
// top-N chunks collapse to fewer than N documents; asking for a multiple of the
// limit makes a full page of distinct files overwhelmingly likely without
// pulling the whole corpus back. A short page is a ranking artefact, never a
// missing document: everything the caller may see is still reachable, just
// possibly below the fold.
const candidateFanout = 8

// semanticSearchIndexed ranks in SQL rather than in Go, ordering by pgvector's
// cosine distance operator so an HNSW index can serve the ordering.
//
// This is not only faster, it is more correct than the scan it replaces. The
// scan bounds its work with maxSemanticScan and an `ORDER BY updated_at DESC`
// truncation, so past that many candidates it ranks the most RECENT vectors
// instead of the most SIMILAR ones — silently, and exactly when a corpus has
// grown enough to need ranking most. Ordering by distance has no such cliff.
//
// It reports ok=false, and no error, when this model's `vec` copy is not yet
// complete. A NULL vec sorts out of an ORDER BY on distance, so ranking that way
// mid-backfill would drop real documents from results; the caller falls back to
// the scan, which reads the packed bytes every row has.
//
// The ACL filter stays exactly where it was — on the node rows, spliced from the
// same Visibility predicate every other query uses. That is the invariant this
// function had to preserve above all: an ANN index that picked its top-N from
// the whole corpus and then filtered would hand a caller an empty page whenever
// the nearest vectors happened to belong to someone else. hnsw.iterative_scan
// is what lets the index keep walking until it has filled the page from rows the
// caller may actually see.
func (s *Store) semanticSearchIndexed(ctx context.Context, ownerID uuid.UUID, query []float32, model string, limit int, includeShared bool) ([]*SearchResult, bool, error) {
	var pending bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM doc_embedding WHERE model = $1 AND dim = $2 AND vec IS NULL)`,
		model, len(query)).Scan(&pending); err != nil {
		return nil, false, err
	}
	if pending {
		return nil, false, nil
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; rollback is the exit

	// Touch a vector value first. pgvector registers hnsw.iterative_scan from
	// its _PG_init, which Postgres runs when the library is first loaded into a
	// backend — and creating the extension does not load it, calling something
	// from it does. On a connection fresh out of the pool the GUC is therefore
	// unknown until this line runs, and the SET below would fail with
	// "unrecognized configuration parameter" on a perfectly good pgvector 0.8.
	if _, err := tx.Exec(ctx, `SELECT '[1]'::vector`); err != nil {
		return nil, false, nil
	}

	// SET LOCAL so the setting dies with this transaction rather than riding a
	// pooled connection into an unrelated query.
	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = relaxed_order`); err != nil {
		// An older pgvector without iterative scan: the ordering is still
		// correct, so this is a recall risk under a selective filter rather
		// than a wrong answer. Falling back to the exact scan is the honest
		// response, since that is what recall it cannot promise.
		return nil, false, nil
	}

	fanout := limit * candidateFanout
	if fanout > maxSemanticScan {
		fanout = maxSemanticScan
	}

	rows, err := tx.Query(ctx, `
		SELECT `+nodeCols+`, de.vec <=> $2::vector AS distance
		`+nodeFrom+`
		JOIN doc_embedding de ON de.content_hash = coalesce(b.sha256, m.content_hash)
			AND de.model = $3 AND de.dim = $4
		WHERE `+Visibility(includeShared)+` AND n.parent_id IS NOT NULL AND n.trashed_at IS NULL
		ORDER BY distance
		LIMIT $5`,
		ownerID, embed.Literal(query), model, len(query), fanout)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	// Rows arrive nearest-first, so the first time a node appears is its best
	// chunk and every later one can be dropped without comparing scores.
	seen := map[uuid.UUID]bool{}
	out := make([]*SearchResult, 0, limit)
	for rows.Next() {
		var (
			n        Node
			size     *int64
			mime     *string
			sha      []byte
			key      *string
			distance float64
		)
		if err := rows.Scan(
			&n.ID, &n.OwnerID, &n.ParentID, &n.Kind, &n.Name, &n.Path,
			&n.HeadVersionID, &n.TrashedAt, &n.TrashedRootID, &n.CreatedAt, &n.UpdatedAt,
			&size, &mime, &sha, &key, &n.ManifestID, &distance,
		); err != nil {
			return nil, false, err
		}
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true

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

		node := n
		// pgvector's <=> is cosine DISTANCE; the rest of the system speaks
		// cosine similarity, and 1 - distance is exactly that, so a score from
		// this path is directly comparable to one from the scan.
		out = append(out, &SearchResult{Node: &node, Score: 1 - distance, Semantic: true})
		if len(out) == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return out, true, nil
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
