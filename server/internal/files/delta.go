package files

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// UserReferencesChunk reports whether any of the user's LIVE files is made of
// this chunk — the authorisation for downloading it.
//
// Chunks are global and shared across users, which is what makes dedup free; but
// that must not become a cross-user READ. Knowing a chunk's hash is not
// permission to fetch it — a user may pull a chunk only if they already hold a
// file assembled from it, which they necessarily do when syncing their own tree.
func (s *Store) UserReferencesChunk(ctx context.Context, ownerID uuid.UUID, hash [32]byte) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM manifest_chunks mc
			JOIN file_versions v ON v.manifest_id = mc.manifest_id
			JOIN nodes n         ON n.id = v.node_id
			WHERE mc.chunk_hash = $1 AND n.owner_id = $2 AND n.trashed_at IS NULL
		)`, hash[:], ownerID).Scan(&exists)
	return exists, err
}

// UserReferencesChunks is the batch form, for the dedup negotiation.
//
// Returns the subset of the given hashes that this user's own live files are
// already made of. Everything else is reported missing even when the server
// holds it — see handleHaveChunks for why that is the point rather than a
// pessimisation.
func (s *Store) UserReferencesChunks(ctx context.Context, ownerID uuid.UUID, hashes [][32]byte) (map[[32]byte]bool, error) {
	keys := make([][]byte, len(hashes))
	for i, h := range hashes {
		keys[i] = h[:]
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT mc.chunk_hash
		FROM manifest_chunks mc
		JOIN file_versions v ON v.manifest_id = mc.manifest_id
		JOIN nodes n         ON n.id = v.node_id
		WHERE mc.chunk_hash = ANY($1) AND n.owner_id = $2 AND n.trashed_at IS NULL`,
		keys, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	held := make(map[[32]byte]bool, len(hashes))
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		var key [32]byte
		copy(key[:], h)
		held[key] = true
	}
	return held, rows.Err()
}

// PutManifestFile records a file whose chunks the client has ALREADY uploaded,
// committing the manifest it names. This is the delta protocol's payoff: the
// bytes never re-transfer — the client chunked locally, uploaded only what the
// server lacked, and here binds those chunks into a file.
//
// It mirrors uploadViaCAS, differing only in who did the chunking. Quota is
// charged on the manifest's logical size by PutFile, so a client cannot inflate
// its allowance by committing chunks it never references — and if PutFile fails,
// a freshly built manifest is dropped, never the shared chunk bytes.
func (s *Service) PutManifestFile(ctx context.Context, ownerID, parentID uuid.UUID, name string, contentHash [32]byte, hashes [][32]byte, declaredMIME string) (*Node, error) {
	if s.cas == nil {
		return nil, fmt.Errorf("content-addressed storage is not configured")
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	manifest, reused, err := s.cas.CommitManifest(ctx, contentHash, hashes)
	if err != nil {
		return nil, err
	}

	node, err := s.store.PutFile(ctx, PutFileInput{
		OwnerID:    ownerID,
		ParentID:   parentID,
		Name:       name,
		ManifestID: manifest.ID,
		Size:       manifest.TotalSize,
		MIME:       DetectMIME(name, declaredMIME),
	})
	if err != nil {
		// Only a manifest we just created is ours to drop; a reused one belongs to
		// another live file. Never the chunk bytes — they may already be shared.
		if !reused {
			if _, delErr := s.cas.DeleteManifestRow(context.WithoutCancel(ctx), manifest.ID); delErr != nil {
				s.log.Warn("could not remove manifest after failed commit",
					"manifest_id", manifest.ID, "error", delErr)
			}
		}
		return nil, err
	}
	s.scheduleExtract(ctx, node)
	return node, nil
}
