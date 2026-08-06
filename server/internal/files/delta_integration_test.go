package files_test

// Phase 3, slice 2: the delta protocol's storage primitives.
//
// The security-critical one is PutChunk: a client writing addressed content must
// have every byte re-hashed and rejected on mismatch, or one client corrupts the
// dedup everyone shares. The rest prove the round trip — upload chunks, commit a
// manifest, reassemble byte-for-byte — and that a commit cannot reference a chunk
// that was never uploaded.

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
)

func TestPutChunkVerifiesAddress(t *testing.T) {
	_, store := casFixture(t)
	data := casData(8<<10, 1)
	good := blake3.Sum256(data)

	if _, err := store.PutChunk(t.Context(), good, data); err != nil {
		t.Fatalf("PutChunk with the correct hash: %v", err)
	}

	// Bytes that do not hash to the claimed address are refused — the whole
	// integrity guarantee of content addressing.
	wrong := good
	wrong[0] ^= 0xff
	if _, err := store.PutChunk(t.Context(), wrong, data); !errors.Is(err, cas.ErrHashMismatch) {
		t.Errorf("PutChunk with a wrong hash = %v, want ErrHashMismatch", err)
	}

	// Anything larger than a chunk can be is rejected before it is hashed.
	big := casData(cas.MaxChunkSize+1, 2)
	if _, err := store.PutChunk(t.Context(), blake3.Sum256(big), big); !errors.Is(err, cas.ErrChunkTooLarge) {
		t.Errorf("PutChunk of an oversized chunk = %v, want ErrChunkTooLarge", err)
	}
}

func TestHasChunksReportsPresence(t *testing.T) {
	_, store := casFixture(t)
	present := casData(6<<10, 3)
	ph := blake3.Sum256(present)
	if _, err := store.PutChunk(t.Context(), ph, present); err != nil {
		t.Fatal(err)
	}
	absent := blake3.Sum256(casData(6<<10, 4)) // never uploaded

	have, err := store.HasChunks(t.Context(), [][32]byte{ph, absent})
	if err != nil {
		t.Fatal(err)
	}
	if !have[ph] {
		t.Error("an uploaded chunk reported missing")
	}
	if have[absent] {
		t.Error("a never-uploaded chunk reported present")
	}
}

// splitToChunks chunks data the way a client would, uploads each plaintext chunk,
// and returns the ordered hashes plus the whole-file hash.
func splitToChunks(t *testing.T, store *cas.Store, data []byte) (hashes [][32]byte, content [32]byte) {
	t.Helper()
	content, err := cas.Split(t.Context(), bytes.NewReader(data), func(c *cas.Chunk) error {
		plain, err := cas.Decompress(c.Stored, c.Compression, c.Size)
		if err != nil {
			return err
		}
		if _, err := store.PutChunk(t.Context(), c.Hash, plain); err != nil {
			return err
		}
		hashes = append(hashes, c.Hash)
		return nil
	})
	if err != nil {
		t.Fatalf("split and upload: %v", err)
	}
	return hashes, content
}

func TestDeltaUploadRoundTrips(t *testing.T) {
	_, store := casFixture(t)
	data := casData(300<<10, 5)

	hashes, content := splitToChunks(t, store, data)
	if len(hashes) < 2 {
		t.Fatalf("expected several chunks, got %d", len(hashes))
	}

	m, reused, err := store.CommitManifest(t.Context(), content, hashes)
	if err != nil {
		t.Fatalf("CommitManifest: %v", err)
	}
	if reused {
		t.Error("a freshly assembled manifest was reported reused")
	}
	if m.TotalSize != int64(len(data)) {
		t.Errorf("manifest total = %d, want %d", m.TotalSize, len(data))
	}

	rc, err := store.Open(t.Context(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("reassembled content differs from the original")
	}
}

func TestCommitRejectsMissingChunk(t *testing.T) {
	_, store := casFixture(t)
	// A hash for content the server was never given.
	phantom := blake3.Sum256(casData(4<<10, 6))
	if _, _, err := store.CommitManifest(t.Context(), phantom, [][32]byte{phantom}); !errors.Is(err, cas.ErrMissingChunk) {
		t.Errorf("commit referencing an unstored chunk = %v, want ErrMissingChunk", err)
	}
}
