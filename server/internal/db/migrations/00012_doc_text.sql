-- +goose Up
-- Phase 4, slice 2: extracted text, content-addressed.
--
-- OCR and text extraction are expensive, so their output is cached by the CONTENT
-- it describes, not by the node: two identical files — a receipt forwarded to two
-- people, the same PDF re-uploaded — are OCR'd once and share the row, exactly as
-- chunks and manifests dedup by content. A new version of a file with new bytes is
-- a new content hash and a new (or already-cached) row; the old text is still
-- there for whoever still holds the old bytes.
--
-- The hash is whatever addresses the content: sha256 for a whole-file blob,
-- blake3 for a chunked file. Which algorithm produced it does not matter here — it
-- is an opaque content identity, joined against coalesce(blobs.sha256,
-- manifests.content_hash) when search folds this text in.

CREATE TABLE doc_text (
    content_hash bytea PRIMARY KEY,

    -- The extracted text. Trigram-indexed below so a scanned receipt becomes
    -- findable by a word printed on it, through the SAME search that already
    -- matches filenames.
    text text NOT NULL,

    -- Best-effort language tag and the character count, for display and for a
    -- cheap "did we get anything" check without measuring the text every time.
    lang  text,
    chars int NOT NULL DEFAULT 0,

    -- Which extractor produced this (e.g. 'text', 'ocr', 'pdf'), so a later,
    -- better extractor can be told which rows are worth redoing.
    source text NOT NULL DEFAULT '',

    extracted_at timestamptz NOT NULL DEFAULT now()
);

-- The index that makes extracted text searchable at the same cost as filenames.
-- GIN over trigrams: the same access path Phase 1 built for names, pointed at the
-- document body.
CREATE INDEX doc_text_trgm ON doc_text USING gin (text gin_trgm_ops);

-- +goose Down
DROP TABLE IF EXISTS doc_text;
