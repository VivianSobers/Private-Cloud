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

// TestCommitVerifiesTheWholeFileHash is the file-level counterpart to
// TestPutChunkVerifiesAddress, and it guards a sharper edge.
//
// content_hash is not a checksum here, it is an IDENTITY: dedup joins on it, and
// doc_text, doc_embedding, media_meta and media_variant are all keyed by it. So a
// commit that could declare an arbitrary hash could claim another file's derived
// content — and, through the reuse branch, that file's manifest outright.
func TestCommitVerifiesTheWholeFileHash(t *testing.T) {
	_, store := casFixture(t)
	data := casData(200<<10, 11)
	hashes, content := splitToChunks(t, store, data)

	wrong := content
	wrong[0] ^= 0xff
	if _, _, err := store.CommitManifest(t.Context(), wrong, hashes); !errors.Is(err, cas.ErrContentHashMismatch) {
		t.Errorf("commit with a wrong content hash = %v, want ErrContentHashMismatch", err)
	}

	// Reordering the same chunks is a different file, and so a different hash.
	// Without whole-file verification this would have passed: the chunks all
	// exist, the total size is identical and so is the count.
	shuffled := make([][32]byte, len(hashes))
	copy(shuffled, hashes)
	shuffled[0], shuffled[len(shuffled)-1] = shuffled[len(shuffled)-1], shuffled[0]
	if _, _, err := store.CommitManifest(t.Context(), content, shuffled); !errors.Is(err, cas.ErrContentHashMismatch) {
		t.Errorf("commit with reordered chunks = %v, want ErrContentHashMismatch", err)
	}

	// The honest commit still works, and still dedups.
	if _, _, err := store.CommitManifest(t.Context(), content, hashes); err != nil {
		t.Fatalf("the correct commit was refused: %v", err)
	}
}

// TestCommitCannotClaimAnotherFilesManifest is the attack the verification
// exists to stop, written out end to end.
//
// The reuse branch matches on (content_hash, total_size, chunk_count). All three
// are readable by anyone who can open the file — GET /nodes/{id}/manifest hands
// them over — and access can be revoked afterwards while the knowledge cannot.
// Chunks of a chosen size are trivial to manufacture, so before this fix the
// three together were enough to be handed the victim's manifest id.
func TestCommitCannotClaimAnotherFilesManifest(t *testing.T) {
	_, store := casFixture(t)

	victim := casData(120<<10, 12)
	victimHashes, victimContent := splitToChunks(t, store, victim)
	victimManifest, _, err := store.CommitManifest(t.Context(), victimContent, victimHashes)
	if err != nil {
		t.Fatal(err)
	}

	// The attacker's own chunks: different bytes, and they do not reassemble to
	// the victim's hash. Their count and total size are irrelevant now, which is
	// the point — the match no longer turns on properties that are cheap to forge.
	attacker := casData(120<<10, 13)
	attackerHashes, _ := splitToChunks(t, store, attacker)

	m, reused, err := store.CommitManifest(t.Context(), victimContent, attackerHashes)
	if !errors.Is(err, cas.ErrContentHashMismatch) {
		t.Fatalf("claiming another file's content hash = (%v, reused=%v, err=%v), want ErrContentHashMismatch",
			m, reused, err)
	}
	if m != nil && m.ID == victimManifest.ID {
		t.Fatal("a forged commit was handed the victim's manifest")
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
