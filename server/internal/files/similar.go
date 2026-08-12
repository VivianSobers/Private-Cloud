package files

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
)

// Similar files, and the retrieval half of RAG (Phase 8).
//
// Both reuse the embedding space Phase 4 already built. No new job kind, no new
// model, no new table: "find documents like this one" and "find documents
// related to this question" are the same operation with a different source
// vector, and pretending otherwise would mean maintaining two nearly identical
// scans that could drift apart in what they consider visible.

// ErrNoEmbedding means the node has not been embedded yet — because extraction
// has not run, because it has no extractable text, or because no sidecar is
// configured. Distinct from "not found": the file exists and the caller may read
// it, there is simply nothing to compare.
var ErrNoEmbedding = errors.New("this file has not been indexed for similarity")

// Chunk is one embedded passage of a document, with the node it came from.
// Retrieval returns chunks rather than whole files because a citation has to
// point at the passage that actually answered the question.
type Chunk struct {
	Node     *Node
	Seq      int
	Text     string
	Score    float64
	Selected bool
}

// SimilarTo ranks the caller's visible files against one file's own vectors.
//
// The source file is excluded from its own results. Returning it first with a
// score of 1.0 is technically correct and useless — nobody asks "what is similar
// to this?" hoping to be told about the thing they are holding.
func (s *Store) SimilarTo(ctx context.Context, userID, nodeID uuid.UUID, model string, limit int, includeShared bool) ([]*SearchResult, error) {
	// The caller must be able to read the source. Without this, a stranger could
	// use any node id as a probe and read the SHAPE of a private document out of
	// the similarity scores it produces.
	if _, err := s.AccessFor(ctx, userID, nodeID); err != nil {
		return nil, err
	}

	vectors, err := s.vectorsForNode(ctx, nodeID, model)
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, ErrNoEmbedding
	}

	// One scan, scored against every source chunk, keeping the best pairing per
	// candidate. A document is "similar" if ANY of its passages is close to ANY
	// of the source's — averaging would let one long, unrelated section drown a
	// genuine match.
	candidates, err := s.scanVectors(ctx, userID, model, len(vectors[0]), includeShared)
	if err != nil {
		return nil, err
	}

	best := map[uuid.UUID]*SearchResult{}
	for _, c := range candidates {
		if c.Node.ID == nodeID {
			continue
		}
		var top float64
		for _, v := range vectors {
			if score := embed.Cosine(v, c.vector); score > top {
				top = score
			}
		}
		if cur, ok := best[c.Node.ID]; ok {
			if top > cur.Score {
				cur.Score = top
			}
			continue
		}
		best[c.Node.ID] = &SearchResult{Node: c.Node, Score: top, Semantic: true}
	}

	out := make([]*SearchResult, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Node.UpdatedAt.After(out[j].Node.UpdatedAt)
	})
	if len(out) > ClampSearchLimit(limit) {
		out = out[:ClampSearchLimit(limit)]
	}
	return out, nil
}

// RetrieveChunks returns the passages most related to a query vector, for RAG.
//
// Chunk-level, not file-level: an answer has to cite the passage it came from,
// and "this 80-page PDF is relevant" is not a citation anybody can check.
//
// scope, when non-empty, restricts retrieval to a subtree — the same ACL-filtered
// candidate set, narrowed further.
func (s *Store) RetrieveChunks(ctx context.Context, userID uuid.UUID, query []float32, model string, limit int, includeShared bool, under string) ([]*Chunk, error) {
	candidates, err := s.scanVectors(ctx, userID, model, len(query), includeShared)
	if err != nil {
		return nil, err
	}

	out := make([]*Chunk, 0, len(candidates))
	for _, c := range candidates {
		if under != "" && c.Node.Path != under && !hasPathPrefix(c.Node.Path, under) {
			continue
		}
		out = append(out, &Chunk{
			Node:  c.Node,
			Seq:   c.seq,
			Score: embed.Cosine(query, c.vector),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	// Passage text is recovered AFTER ranking, and only for the handful of chunks
	// that survived. It is not stored beside the vector: doc_text already holds
	// the full text content-addressed, and ChunkText is deterministic, so the
	// passage is exactly ChunkText(text)[seq]. Duplicating it into doc_embedding
	// would double the storage for every document and add a second copy that
	// could drift from the first when the chunker changes.
	if err := s.fillChunkText(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fillChunkText recovers each chunk's passage by re-chunking its document.
//
// One doc_text read per distinct document, not per chunk — a question that
// retrieves five passages from one long PDF should read that PDF's text once.
func (s *Store) fillChunkText(ctx context.Context, chunks []*Chunk) error {
	cache := map[string][]string{}
	for _, c := range chunks {
		key := string(c.Node.ContentHash)
		parts, ok := cache[key]
		if !ok {
			text, found, err := s.DocText(ctx, c.Node.ContentHash)
			if err != nil {
				return err
			}
			if found {
				parts = embed.ChunkText(text)
			}
			cache[key] = parts
		}
		if c.Seq >= 0 && c.Seq < len(parts) {
			c.Text = parts[c.Seq]
		}
	}
	return nil
}

// scannedChunk is one row of the vector scan.
type scannedChunk struct {
	Node   *Node
	seq    int
	vector []float32
}

// scanVectors is the one place the ACL filter meets the vector store.
//
// The filter is on the NODE rows and never on the vectors, for the reason
// SemanticSearch documents: embeddings are content-addressed, so two users
// owning the same document share one vector row by construction. Every Phase 8
// feature goes through here so none of them can get that wrong independently.
func (s *Store) scanVectors(ctx context.Context, userID uuid.UUID, model string, dim int, includeShared bool) ([]scannedChunk, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+`, de.chunk_seq, de.vector
		`+nodeFrom+`
		JOIN doc_embedding de ON de.content_hash = coalesce(b.sha256, m.content_hash)
			AND de.model = $2 AND de.dim = $3
		WHERE `+Visibility(includeShared)+`
		  AND n.parent_id IS NOT NULL
		  AND n.trashed_at IS NULL
		ORDER BY n.updated_at DESC, n.id, de.chunk_seq
		LIMIT $4`, userID, model, dim, maxSemanticScan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]scannedChunk, 0, 256)
	for rows.Next() {
		var (
			n    Node
			size *int64
			mime *string
			sha  []byte
			key  *string
			seq  int
			vec  []byte
		)
		if err := rows.Scan(
			&n.ID, &n.OwnerID, &n.ParentID, &n.Kind, &n.Name, &n.Path,
			&n.HeadVersionID, &n.TrashedAt, &n.TrashedRootID, &n.CreatedAt, &n.UpdatedAt,
			&size, &mime, &sha, &key, &n.ManifestID, &seq, &vec,
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
		node := n
		out = append(out, scannedChunk{Node: &node, seq: seq, vector: embed.Unpack(vec)})
	}
	return out, rows.Err()
}

// vectorsForNode loads one node's own vectors, to compare everything else
// against.
func (s *Store) vectorsForNode(ctx context.Context, nodeID uuid.UUID, model string) ([][]float32, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT de.vector
		FROM nodes n
		JOIN file_versions v ON v.id = n.head_version_id
		LEFT JOIN blobs b     ON b.id = v.blob_id
		LEFT JOIN manifests m ON m.id = v.manifest_id
		JOIN doc_embedding de ON de.content_hash = coalesce(b.sha256, m.content_hash)
			AND de.model = $2
		WHERE n.id = $1 AND n.trashed_at IS NULL
		ORDER BY de.chunk_seq`, nodeID, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][]float32
	for rows.Next() {
		var vec []byte
		if err := rows.Scan(&vec); err != nil {
			return nil, err
		}
		out = append(out, embed.Unpack(vec))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// hasPathPrefix reports whether path lies under prefix, comparing whole
// segments. "/projectX" is not under "/project".
func hasPathPrefix(path, prefix string) bool {
	if len(path) <= len(prefix) {
		return false
	}
	return path[:len(prefix)] == prefix && path[len(prefix)] == '/'
}
