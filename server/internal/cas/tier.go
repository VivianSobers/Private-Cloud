package cas

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
)

// WarmManifest makes sure every chunk of a manifest is on the local pool before
// anybody starts reading it.
//
// This is the part of the cold tier that a chunked file needs and a whole-file
// blob does not. manifestReader loads one chunk at a time, lazily, so a cold
// chunk in the middle of a video would be discovered by ServeContent AFTER the
// 200 and the first megabyte had already gone out — and the only thing left to
// do at that point is to hang up mid-file. There is no status code available
// once the response has begun.
//
// So the question is asked once, up front, in ONE query: does this manifest
// reference any chunk the database says is cold? On the overwhelmingly common
// answer — no — it costs an indexed count and nothing else. On a yes, every
// cold chunk is queued for promotion and the caller is told to come back, which
// the HTTP layer renders as 202 restore_in_progress.
//
// It is a no-op when the blob store is not tiered, which keeps every deployment
// without a cold tier on exactly the code path it had before.
func (s *Store) WarmManifest(ctx context.Context, manifestID uuid.UUID) error {
	tiered, ok := s.blobs.(*blob.TieredStore)
	if !ok || !tiered.Enabled() {
		return nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT c.storage_key
		FROM manifest_chunks mc
		JOIN chunks c ON c.hash = mc.chunk_hash
		WHERE mc.manifest_id = $1 AND c.tier <> 'hot'`, manifestID)
	if err != nil {
		return fmt.Errorf("check manifest tiers: %w", err)
	}
	defer rows.Close()

	var cold []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return err
		}
		cold = append(cold, key)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(cold) == 0 {
		return nil
	}

	// Start them ALL before waiting on any. A restore that warms one chunk at a
	// time makes a file with twenty cold chunks twenty sequential transfers, and
	// the last one would not begin moving until the client had already waited
	// out the nineteen before it.
	for _, key := range cold {
		tiered.StartRestore(key)
	}

	var restoring error
	for _, key := range cold {
		err := tiered.Warm(ctx, key)
		switch {
		case err == nil:
		case errors.Is(err, blob.ErrRestoring):
			// Keep going: the point of this loop is to have all of them in
			// flight at once. Report the wait after they have all been started.
			restoring = err
		default:
			return err
		}
	}
	return restoring
}
