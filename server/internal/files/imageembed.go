package files

import (
	"bytes"
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
)

// The image-embedding space (Phase 8, slice 5): storage, and the adapter that
// joins the embed package's image handler to this one.
//
// files depends on embed and on media, never the reverse — the same rule
// extractadapter.go and mediaadapter.go follow. The handler stays pure of the
// database and the blob store so it can be tested on bytes alone, and this
// package, which already knows how to open content, supplies the glue.

// NewImageEmbedOpener adapts the file service to the image handler's opener.
//
// A separate opener from NewMediaOpener even though both call svc.Open, because
// the two live in different packages and neither may import the other's
// sentinel. The conversion is four lines in one place, which is the price of the
// dependency running one way.
func NewImageEmbedOpener(svc *Service) embed.ImageOpener { return imageOpener{svc: svc} }

type imageOpener struct{ svc *Service }

func (o imageOpener) OpenForImageEmbed(ctx context.Context, ownerID, nodeID uuid.UUID) (embed.ImageContent, error) {
	node, rc, err := o.svc.Open(ctx, ownerID, nodeID)
	if errors.Is(err, ErrNotFound) {
		// Trashed or purged between enqueue and now.
		return embed.ImageContent{}, embed.ErrImageGone
	}
	if err != nil {
		return embed.ImageContent{}, err
	}
	return embed.ImageContent{
		MIME:        node.MIME,
		ContentHash: node.ContentHash,
		Reader:      rc,
	}, nil
}

// NewImageVectorStore adapts this package's store to the handler's.
func NewImageVectorStore(s *Store) embed.ImageVectorStore { return s }

// imageVectorReady reports whether image_embedding has the pgvector column.
//
// Probed per table rather than inferred from doc_embedding's answer. Migrations
// 00026 and 00027 add their columns under the identical condition, so the two
// answers agree on every database that migrated in one pass — but a database
// that reached 00026 without pgvector, gained the extension, and then reached
// 00027 has one column and not the other. Asking the catalog about the table
// actually being written to is asking the only thing that knows, and the cost is
// one query per process.
func (s *Store) imageVectorReady(ctx context.Context) bool {
	s.imgVecOnce.Do(func() {
		var ok bool
		err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'image_embedding' AND column_name = 'vec'
			)`).Scan(&ok)
		// A probe that errors has not proved the column is there, and the packed
		// bytes are always correct — so failure degrades rather than propagating.
		s.imgVec = err == nil && ok
	})
	return s.imgVec
}

// HasImageEmbedding reports whether a content hash is already embedded in a
// model's image space, so the worker skips content it has already processed —
// and, because this is content-addressed, skips the second upload of a picture
// it embedded the first time.
func (s *Store) HasImageEmbedding(ctx context.Context, contentHash []byte, model string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM image_embedding WHERE content_hash = $1 AND model = $2)`,
		contentHash, model).Scan(&exists)
	return exists, err
}

// PutImageEmbedding stores one image's vector for one model.
//
// An upsert rather than an insert: re-running the job over a photo must converge
// on the same single row rather than failing on the primary key, which is what
// makes `cloudctl jobs reindex --kind=image_embed` safe to run at any time.
//
// `vec` is written alongside the packed bytes where the column exists, so the
// pgvector copy never lags the source of truth for anything written after
// migration 00027 — the property that lets `cloudctl embeddings index` refuse to
// build over a half-filled column and still find it full.
func (s *Store) PutImageEmbedding(ctx context.Context, contentHash []byte, model string, dim int, vector []float32) error {
	packed := embed.Pack(vector)

	if s.imageVectorReady(ctx) {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO image_embedding (content_hash, model, dim, vector, vec)
			VALUES ($1, $2, $3, $4, $5::vector)
			ON CONFLICT (content_hash, model)
			DO UPDATE SET dim = EXCLUDED.dim, vector = EXCLUDED.vector,
			              vec = EXCLUDED.vec, created_at = now()`,
			contentHash, model, dim, packed, embed.Literal(vector))
		return err
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO image_embedding (content_hash, model, dim, vector)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (content_hash, model)
		DO UPDATE SET dim = EXCLUDED.dim, vector = EXCLUDED.vector, created_at = now()`,
		contentHash, model, dim, packed)
	return err
}

// ImageEmbeddingFor returns one content hash's vector, for tests and for the
// operator tooling. Absent is not an error: most files are not images.
func (s *Store) ImageEmbeddingFor(ctx context.Context, contentHash []byte, model string) ([]float32, bool, error) {
	var packed []byte
	err := s.pool.QueryRow(ctx,
		`SELECT vector FROM image_embedding WHERE content_hash = $1 AND model = $2`,
		contentHash, model).Scan(&packed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(packed) == 0 {
		return nil, false, nil
	}
	return embed.Unpack(bytes.Clone(packed)), true, nil
}

// PruneImageEmbeddings deletes vectors for content no live version references,
// bounded — the image half of what PruneEmbeddings does for documents. Nothing
// else ever deletes these rows: they are keyed by content hash rather than by
// node, so no cascade reaches them when a file is purged.
func (s *Store) PruneImageEmbeddings(ctx context.Context, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM image_embedding
		WHERE content_hash IN (
			SELECT ie.content_hash FROM image_embedding ie
			WHERE NOT EXISTS (
				SELECT 1 FROM file_versions v
				LEFT JOIN blobs b     ON b.id = v.blob_id
				LEFT JOIN manifests m ON m.id = v.manifest_id
				WHERE coalesce(b.sha256, m.content_hash) = ie.content_hash
			)
			LIMIT $1
		)`, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
