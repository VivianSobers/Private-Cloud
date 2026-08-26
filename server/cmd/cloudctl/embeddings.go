package main

import (
	"context"
	"fmt"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/embed"
)

// `cloudctl embeddings` — the pgvector upgrade, run by an operator rather than
// by a migration.
//
// Migration 00026 adds doc_embedding.vec where the extension exists, but it
// deliberately does not fill it or index it. Filling it means decoding every
// stored vector, and indexing it means choosing a width — neither belongs in a
// migration that runs automatically on startup while the API is trying to come
// up. Both are done here, on purpose, at a moment the operator picked.
//
// The order is not negotiable and the code enforces it: backfill, verify, then
// index. An HNSW index over a half-filled column is an index that silently omits
// rows, which is the one failure mode worse than being slow.

// backfillBatch is how many rows one statement converts. Small enough that the
// work is interruptible and never holds a long transaction against the table the
// API is reading, large enough that a big corpus does not become a million round
// trips.
const backfillBatch = 2000

func embeddingsCommand(ctx context.Context, database *db.DB, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cloudctl embeddings <status|backfill|index>")
	}

	ready, err := pgvectorColumn(ctx, database)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("pgvector is not installed on this database, so there is nothing to backfill; " +
			"semantic search is using the exact scan and is correct as it stands")
	}

	switch args[0] {
	case "status":
		return embeddingsStatus(ctx, database)
	case "backfill":
		return embeddingsBackfill(ctx, database)
	case "index":
		return embeddingsIndex(ctx, database)
	default:
		return fmt.Errorf("unknown embeddings command %q", args[0])
	}
}

func pgvectorColumn(ctx context.Context, database *db.DB) (bool, error) {
	var ok bool
	err := database.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'doc_embedding' AND column_name = 'vec'
		)`).Scan(&ok)
	return ok, err
}

// embeddingsStatus reports per (model, dim) how much of the copy is done and
// whether the index for that width exists — the two facts that decide which path
// a search actually takes.
func embeddingsStatus(ctx context.Context, database *db.DB) error {
	rows, err := database.Pool.Query(ctx, `
		SELECT model, dim, count(*), count(*) FILTER (WHERE vec IS NULL)
		FROM doc_embedding
		GROUP BY model, dim
		ORDER BY model, dim`)
	if err != nil {
		return err
	}
	defer rows.Close()

	any := false
	for rows.Next() {
		var (
			model          string
			dim            int
			total, pending int64
		)
		if err := rows.Scan(&model, &dim, &total, &pending); err != nil {
			return err
		}
		any = true

		indexed, err := hasHNSWIndex(ctx, database, dim)
		if err != nil {
			return err
		}

		state := "exact scan"
		switch {
		case pending > 0:
			state = fmt.Sprintf("backfill incomplete (%d of %d rows left)", pending, total)
		case !indexed:
			state = "ranked in SQL, not indexed — run `cloudctl embeddings index`"
		default:
			state = "indexed"
		}
		fmt.Printf("%s (dim %d): %d vectors, %s\n", model, dim, total, state)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !any {
		fmt.Println("no embeddings stored yet")
	}
	return nil
}

// embeddingsBackfill decodes the packed bytes of every row whose vec is still
// NULL and writes the pgvector copy.
//
// The decode happens in Go rather than in SQL because the stored form is
// little-endian packed float32 and Postgres has no native way to read that back
// — float4recv is big-endian. Going through embed.Unpack and embed.Literal means
// the copy is produced by exactly the code that produced the original, so the
// two ranking paths cannot drift on the wire format.
func embeddingsBackfill(ctx context.Context, database *db.DB) error {
	var converted int64
	for {
		rows, err := database.Pool.Query(ctx, `
			SELECT content_hash, chunk_seq, model, vector
			FROM doc_embedding
			WHERE vec IS NULL
			LIMIT $1`, backfillBatch)
		if err != nil {
			return err
		}

		type row struct {
			hash  []byte
			seq   int
			model string
			vec   []byte
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.hash, &r.seq, &r.model, &r.vec); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}

		for _, r := range batch {
			if _, err := database.Pool.Exec(ctx, `
				UPDATE doc_embedding SET vec = $4::vector
				WHERE content_hash = $1 AND chunk_seq = $2 AND model = $3`,
				r.hash, r.seq, r.model, embed.Literal(embed.Unpack(r.vec))); err != nil {
				return err
			}
			converted++
		}
		fmt.Printf("converted %d vectors\n", converted)
	}

	fmt.Printf("backfill complete: %d vectors converted\n", converted)
	return nil
}

// embeddingsIndex builds one HNSW index per stored width, and refuses to build
// any of them while that width still has rows to convert.
//
// One index per width rather than one for the table, because doc_embedding holds
// several models side by side by design and their vectors are different shapes.
// The index is a partial expression index — over `vec::vector(N)` where
// `dim = N` — which is what lets a single mixed-width column still be served by
// a real ANN index.
func embeddingsIndex(ctx context.Context, database *db.DB) error {
	rows, err := database.Pool.Query(ctx, `
		SELECT dim, count(*) FILTER (WHERE vec IS NULL)
		FROM doc_embedding GROUP BY dim ORDER BY dim`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type width struct {
		dim     int
		pending int64
	}
	var widths []width
	for rows.Next() {
		var w width
		if err := rows.Scan(&w.dim, &w.pending); err != nil {
			return err
		}
		widths = append(widths, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(widths) == 0 {
		fmt.Println("no embeddings stored yet; nothing to index")
		return nil
	}

	for _, w := range widths {
		if w.pending > 0 {
			return fmt.Errorf("dim %d still has %d unconverted vectors; "+
				"run `cloudctl embeddings backfill` first — an index built now would omit them",
				w.dim, w.pending)
		}
	}

	for _, w := range widths {
		name := fmt.Sprintf("doc_embedding_hnsw_%d", w.dim)
		// The width is an int read from the table's own dim column, never
		// caller input, so interpolating it into DDL cannot carry anything but
		// a number — and an index definition cannot take a placeholder.
		stmt := fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON doc_embedding
			 USING hnsw ((vec::vector(%d)) vector_cosine_ops) WHERE dim = %d`,
			name, w.dim, w.dim)
		fmt.Printf("building %s ...\n", name)
		if _, err := database.Pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("build %s: %w", name, err)
		}
	}
	fmt.Println("indexes built")
	return nil
}

func hasHNSWIndex(ctx context.Context, database *db.DB, dim int) (bool, error) {
	var ok bool
	err := database.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'doc_embedding' AND indexname = $1)`,
		fmt.Sprintf("doc_embedding_hnsw_%d", dim)).Scan(&ok)
	return ok, err
}
