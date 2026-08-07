package files

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Extracted document text, content-addressed. The worker writes it after OCR or a
// text/PDF extraction; search reads it by joining a node's content hash. Keying by
// content means identical files are extracted once and share the row — the same
// dedup discipline as chunks and manifests, applied to their text.

// PutDocText records extracted text for a content hash, replacing any prior text
// for the same content (a better extractor re-running, say). Idempotent by
// content, so a job that runs twice writes the same row twice harmlessly.
func (s *Store) PutDocText(ctx context.Context, contentHash []byte, text, lang, source string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO doc_text (content_hash, text, lang, chars, source, extracted_at)
		VALUES ($1, $2, $3, char_length($2), $4, now())
		ON CONFLICT (content_hash) DO UPDATE SET
			text = excluded.text, lang = excluded.lang,
			chars = excluded.chars, source = excluded.source, extracted_at = now()`,
		contentHash, text, nullIfEmpty(lang), source)
	return err
}

// HasDocText reports whether text is already cached for a content hash, so the
// worker can skip re-extracting content it has already processed.
func (s *Store) HasDocText(ctx context.Context, contentHash []byte) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM doc_text WHERE content_hash = $1)`, contentHash).Scan(&exists)
	return exists, err
}

// DocText returns the stored text for a content hash, for tests and any future
// "show me what was extracted" view.
func (s *Store) DocText(ctx context.Context, contentHash []byte) (string, bool, error) {
	var text string
	err := s.pool.QueryRow(ctx,
		`SELECT text FROM doc_text WHERE content_hash = $1`, contentHash).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// PruneDocText deletes extracted text no live file version references any more —
// content that has been fully deleted and garbage-collected. Bounded so a large
// backlog does not lock the table.
func (s *Store) PruneDocText(ctx context.Context, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM doc_text
		WHERE content_hash IN (
			SELECT dt.content_hash FROM doc_text dt
			WHERE NOT EXISTS (
				SELECT 1 FROM file_versions v
				LEFT JOIN blobs b     ON b.id = v.blob_id
				LEFT JOIN manifests m ON m.id = v.manifest_id
				WHERE coalesce(b.sha256, m.content_hash) = dt.content_hash
			)
			LIMIT $1
		)`, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
