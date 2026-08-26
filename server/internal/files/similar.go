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
//
// Slice 5 added a SECOND space — image embeddings — and deliberately did not add
// a second scan. A photograph with no text has no document vector, so the only
// thing that changes is which table the source vector and the candidates come
// out of; everything after that, including the ACL filter, is the same code.

// ErrNoEmbedding means the node has not been embedded yet — because extraction
// has not run, because it has no extractable text, or because no sidecar is
// configured. Distinct from "not found": the file exists and the caller may read
// it, there is simply nothing to compare.
var ErrNoEmbedding = errors.New("this file has not been indexed for similarity")

// The two vector spaces this layer can rank in, and the names of the routing
// answers a caller gets back.
const (
	SpaceText  = "text"
	SpaceImage = "image"
)

// vectorSpace names one of the embedding tables.
//
// The table name is interpolated into SQL, which is safe only because these two
// values are the complete set and both are compile-time constants in this file.
// Nothing derived from a request ever reaches here, and nothing should: the
// alternative — a caller naming its own table — is how a query layer becomes an
// injection surface.
type vectorSpace struct {
	table string
	// hasSeq is true for a table whose rows are passages of one document. An
	// image is one thing, so its space has no chunk sequence and the retrieval
	// layer reads a constant zero instead of a column that would only ever hold
	// one value.
	hasSeq bool
}

var (
	docSpace   = vectorSpace{table: "doc_embedding", hasSeq: true}
	imageSpace = vectorSpace{table: "image_embedding"}
)

// seqCol is the SELECT expression yielding a row's chunk sequence.
func (v vectorSpace) seqCol() string {
	if v.hasSeq {
		return "e.chunk_seq"
	}
	return "0"
}

// SimilarSpaces names the vector spaces a similarity query is allowed to rank
// in. An empty model means that space is not configured on this server, which is
// the ordinary state: most deployments run one sidecar, several run none.
type SimilarSpaces struct {
	// Text is the document-embedding model identity.
	Text string
	// Image is the image-embedding model identity.
	Image string
}

// SimilarTo ranks the caller's visible files against one file's own vectors, and
// returns the space it ranked in.
//
// The source file is excluded from its own results. Returning it first with a
// score of 1.0 is technically correct and useless — nobody asks "what is similar
// to this?" hoping to be told about the thing they are holding.
//
// Routing between the two spaces happens HERE, after the access check, and not
// in the handler. A handler that first asked "is this node in the image space?"
// and then asked for results would be answering that first question for a node
// the caller may not be allowed to know exists — the same probe the access check
// below exists to close, reopened one layer up.
//
// The rule is: rank in the image space when the source has an image vector in a
// configured image space, otherwise rank in the text space exactly as before. A
// photograph that has both — a scan with readable text, say — prefers the image
// space, because the picture is what the person is looking at. A photograph with
// no image vector but with text neighbours is unchanged from what it did before
// this space existed, which is the property that makes the feature strictly
// additive.
func (s *Store) SimilarTo(ctx context.Context, userID, nodeID uuid.UUID, spaces SimilarSpaces, limit int, includeShared bool) ([]*SearchResult, string, error) {
	// The caller must be able to read the source. Without this, a stranger could
	// use any node id as a probe and read the SHAPE of a private document out of
	// the similarity scores it produces.
	if _, err := s.AccessFor(ctx, userID, nodeID); err != nil {
		return nil, "", err
	}

	if spaces.Image != "" {
		vectors, err := s.vectorsForNode(ctx, imageSpace, nodeID, spaces.Image)
		if err != nil {
			return nil, "", err
		}
		if len(vectors) > 0 {
			out, err := s.rankAgainst(ctx, userID, imageSpace, spaces.Image, nodeID, vectors, limit, includeShared)
			return out, SpaceImage, err
		}
	}

	if spaces.Text == "" {
		return nil, "", ErrNoEmbedding
	}
	vectors, err := s.vectorsForNode(ctx, docSpace, nodeID, spaces.Text)
	if err != nil {
		return nil, "", err
	}
	if len(vectors) == 0 {
		return nil, "", ErrNoEmbedding
	}
	out, err := s.rankAgainst(ctx, userID, docSpace, spaces.Text, nodeID, vectors, limit, includeShared)
	return out, SpaceText, err
}

// rankAgainst scores every visible candidate in one space against the source's
// own vectors, best pairing per candidate.
//
// One scan, one scoring rule, whichever space is in play. A file is "similar" if
// ANY of its passages is close to ANY of the source's — averaging would let one
// long, unrelated section drown a genuine match. In the image space each side
// has exactly one vector, so the rule collapses to a single cosine and costs
// nothing to share.
func (s *Store) rankAgainst(ctx context.Context, userID uuid.UUID, space vectorSpace, model string, exclude uuid.UUID, vectors [][]float32, limit int, includeShared bool) ([]*SearchResult, error) {
	candidates, err := s.scanVectors(ctx, space, userID, model, len(vectors[0]), includeShared, "", FeedbackSimilar)
	if err != nil {
		return nil, err
	}

	best := map[uuid.UUID]*SearchResult{}
	for _, c := range candidates {
		if c.Node.ID == exclude {
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

// RetrieveChunks returns the passages most related to a query vector, for RAG.
//
// Chunk-level, not file-level: an answer has to cite the passage it came from,
// and "this 80-page PDF is relevant" is not a citation anybody can check.
//
// Document space only, deliberately. An image vector has no text to hand a
// generator, and citing a photograph as the source of a sentence would be an
// invention wearing a citation's clothes.
//
// scope, when non-empty, restricts retrieval to a subtree — the same ACL-filtered
// candidate set, narrowed further.
func (s *Store) RetrieveChunks(ctx context.Context, userID uuid.UUID, query []float32, model string, limit int, includeShared bool, under string) ([]*Chunk, error) {
	candidates, err := s.scanVectors(ctx, docSpace, userID, model, len(query), includeShared, under, FeedbackCitation)
	if err != nil {
		return nil, err
	}

	out := make([]*Chunk, 0, len(candidates))
	for _, c := range candidates {
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
// owning the same document — or the same photograph — share one vector row by
// construction. Every Phase 8 feature goes through here so none of them can get
// that wrong independently, and adding the image space in slice 5 meant giving
// this function a table rather than giving the image space a scan of its own:
// two nearly identical scans drift, and for an ACL filter drift means a leak.
//
// under, when non-empty, restricts the scan to one subtree. It is applied in
// SQL, BEFORE the limit, and that ordering is the whole point of the parameter
// existing here rather than as a filter in Go. The limit takes the most recently
// updated maxSemanticScan chunks; filtering afterwards meant a question scoped to
// an older subtree on a large library matched nothing at all, and answered
// "no_matching_documents" — which is indistinguishable from the subtree
// genuinely having no answer, so nobody would think to look further.
//
// starts_with with a trailing separator, not LIKE, for the reason the grant
// predicate documents at length: the pattern would be built from a column, so a
// scope of /100%_done would otherwise also cover /1009Xdone.
//
// suppressKind names the feedback kind whose 'wrong' verdicts remove a node from
// this scan for this caller (Phase 8, open item 9). It rides in here rather than
// filtering afterwards for the same reason `under` does: the LIMIT truncates by
// updated_at, and a candidate dropped in Go has already spent a slot in the scan
// on something the caller has told us they do not want. Empty suppresses nothing.
func (s *Store) scanVectors(ctx context.Context, space vectorSpace, userID uuid.UUID, model string, dim int, includeShared bool, under, suppressKind string) ([]scannedChunk, error) {
	// Deterministic truncation, so the bound below cuts the same rows every run
	// rather than leaving the planner to decide once a corpus exceeds the cap.
	// The chunk sequence is only part of the ordering where it is a real column;
	// a bare "0" in ORDER BY would be read as an ordinal reference, not a value.
	order := "ORDER BY n.updated_at DESC, n.id"
	if space.hasSeq {
		order += ", e.chunk_seq"
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+`, `+space.seqCol()+`, e.vector
		`+nodeFrom+`
		JOIN `+space.table+` e ON e.content_hash = coalesce(b.sha256, m.content_hash)
			AND e.model = $2 AND e.dim = $3
		WHERE `+Visibility(includeShared)+`
		  AND n.parent_id IS NOT NULL
		  AND n.trashed_at IS NULL
		  AND ($5 = '' OR n.path = $5 OR starts_with(n.path, $5 || '/'))
		  AND `+NotMarkedWrong("$1", "$6")+`
		`+order+`
		LIMIT $4`, userID, model, dim, maxSemanticScan, under, suppressKind)
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

// vectorsForNode loads one node's own vectors in one space, to compare
// everything else against. Empty is the ordinary answer, not an error: it is how
// "this file is not in this space" is expressed, and it is what routes a
// photograph with no image vector back to the text space.
func (s *Store) vectorsForNode(ctx context.Context, space vectorSpace, nodeID uuid.UUID, model string) ([][]float32, error) {
	order := ""
	if space.hasSeq {
		order = "ORDER BY e.chunk_seq"
	}

	rows, err := s.pool.Query(ctx, `
		SELECT e.vector
		FROM nodes n
		JOIN file_versions v ON v.id = n.head_version_id
		LEFT JOIN blobs b     ON b.id = v.blob_id
		LEFT JOIN manifests m ON m.id = v.manifest_id
		JOIN `+space.table+` e ON e.content_hash = coalesce(b.sha256, m.content_hash)
			AND e.model = $2
		WHERE n.id = $1 AND n.trashed_at IS NULL
		`+order, nodeID, model)
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
