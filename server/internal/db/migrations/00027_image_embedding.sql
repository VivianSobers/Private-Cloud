-- +goose Up
-- Phase 8, slice 5: image embeddings — the second vector space.
--
-- /nodes/{id}/similar has only ever ranked in the DOCUMENT space, so two
-- photographs with no extractable text have no neighbours at all: nothing in
-- doc_embedding describes them, and nothing ever will, because extraction is
-- about words. A photo library is exactly the corpus "find files like this one"
-- was wanted for, and it was the one corpus the feature could not serve.
--
-- Content-addressed by content hash, like doc_embedding and like every media
-- derivation: the vector describes the BYTES, so the same picture uploaded twice
-- is embedded once and both nodes share the row. On this hardware that is not a
-- micro-optimisation — a forward pass through a vision encoder is the expensive
-- thing this tier does.
--
-- Deliberately NOT per-owner, unlike faces (migration 00023). A face vector
-- describes a person and belongs to whoever's library it was found in; an image
-- vector describes the picture itself, exactly as a document vector describes a
-- document. The ACL filter therefore stays where it already is — on the node
-- rows, in scanVectors — and never on the vectors.
--
-- One vector per image, so there is no chunk_seq here. A document is several
-- passages because a query can be about one paragraph of it; an image is one
-- thing. Padding a constant zero into the key to make the two tables identical
-- would be a column that only ever holds one value and a promise the schema
-- cannot keep.
--
-- `model` is part of the key for doc_embedding's reason: vectors from two models
-- are not comparable, so a model change writes alongside rather than over, and
-- ranking filters to the model it is querying with.

CREATE TABLE image_embedding (
    content_hash bytea NOT NULL,
    model        text  NOT NULL,
    dim          int   NOT NULL CHECK (dim > 0),
    vector       bytea NOT NULL CHECK (octet_length(vector) = dim * 4),
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (content_hash, model)
);

-- Ranking scans one model's vectors and joins them back to live files by content
-- hash; this index makes that scan touch only the model in play. The primary key
-- already leads with content_hash, which is the wrong order for that scan.
CREATE INDEX image_embedding_model ON image_embedding (model, content_hash);

-- The pgvector half, on exactly the terms migration 00026 established: the
-- extension is optional, so this succeeds on stock Postgres and simply leaves
-- the column absent. The reason is the same one — the nightly pg_dumpall has to
-- restore onto a bare machine, and runbook-restore.md leans on that being true
-- at the worst possible moment.
--
-- `vec` is a derived copy of the packed bytes, never the source of truth, and
-- the width is left off the type for 00026's reason: several models may live
-- here side by side and their widths differ, so the ANN indexes are per-width
-- partial expression indexes built by `cloudctl embeddings index` rather than a
-- vector(N) column that would pin one model forever.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        ALTER TABLE image_embedding ADD COLUMN IF NOT EXISTS vec vector;

        -- The pending set, exactly as doc_embedding_vec_pending: one entry per
        -- un-backfilled row and nothing at all once the backfill finishes. A
        -- NULL vec sorts out of an ORDER BY on distance, so ranking in SQL
        -- before the copy is complete would silently DROP images from results
        -- rather than merely being slower. Anything that wants the indexed path
        -- checks this first.
        CREATE INDEX IF NOT EXISTS image_embedding_vec_pending
            ON image_embedding (model, dim) WHERE vec IS NULL;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS image_embedding;
