-- +goose Up
-- Phase 4, slice 3: document embeddings for semantic search.
--
-- Content-addressed like doc_text: the vector describes the CONTENT, so identical
-- files are embedded once and share the row. A long document is several chunks,
-- one vector each, so a query can match the paragraph that is actually about it
-- rather than diluting the whole file into one average vector.
--
-- The vector is stored as packed little-endian float32 (dim*4 bytes), not a
-- pgvector column: the stock Postgres image has no pgvector, and at personal
-- scale — thousands of documents — an exact cosine scan in the application is
-- fast and needs no extension. pgvector with an HNSW index is the clean upgrade
-- when a corpus grows past that, and the query layer is written to swap to it
-- without changing this table's shape.
--
-- `model` is part of the key: switching embedding models must not compare vectors
-- from different spaces, so each model's vectors live alongside rather than
-- overwriting, and search filters to the model it is querying with.

CREATE TABLE doc_embedding (
    content_hash bytea NOT NULL,
    chunk_seq    int   NOT NULL,
    model        text  NOT NULL,
    dim          int   NOT NULL CHECK (dim > 0),
    vector       bytea NOT NULL CHECK (octet_length(vector) = dim * 4),
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (content_hash, model, chunk_seq)
);

-- Search scans one model's vectors and joins them back to live files by content
-- hash; this index makes that scan touch only the model in play.
CREATE INDEX doc_embedding_model ON doc_embedding (model, content_hash);

-- +goose Down
DROP TABLE IF EXISTS doc_embedding;
