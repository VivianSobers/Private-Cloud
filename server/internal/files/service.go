package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
)

// Service joins the node store to the blob store.
//
// The split matters: Store knows nothing about bytes and blob.Store knows
// nothing about users or trees. Phase 2 swaps the blob implementation for
// content-defined chunking without this file changing much, and nothing in the
// HTTP layer at all.
type Service struct {
	store *Store
	blobs blob.Store
	log   *slog.Logger

	// stager is non-nil when the blob store supports resumable uploads. Held
	// as an interface rather than a concrete type so a Phase 2 backend can
	// provide resumability its own way.
	stager blob.Stager

	// cas is non-nil once content-addressed storage is configured. It is
	// optional because whole-file blobs remain a supported, permanent format —
	// but fsck and GC must consult it whenever it exists, or they will treat
	// every chunk on disk as garbage.
	cas *cas.Store

	// TrashRetention bounds how long deleted files keep consuming disk.
	TrashRetention time.Duration
	// BlobGCGrace keeps a newly written blob safe from GC while the transaction
	// that references it is still in flight.
	BlobGCGrace time.Duration
	// UploadTTL bounds how long an abandoned resumable upload occupies disk.
	UploadTTL time.Duration
}

func NewService(store *Store, blobs blob.Store, log *slog.Logger) *Service {
	s := &Service{
		store:          store,
		blobs:          blobs,
		log:            log,
		TrashRetention: 30 * 24 * time.Hour,
		BlobGCGrace:    time.Hour,
		UploadTTL:      defaultUploadTTL,
	}
	if st, ok := blobs.(blob.Stager); ok {
		s.stager = st
	}
	return s
}

// SetCAS attaches a content-addressed store.
//
// Wiring this in is what makes fsck and GC aware that chunks exist. Uploads do
// not yet route through it — that is the remaining half of Phase 2 slice 1 —
// but the maintenance paths must understand the format BEFORE anything writes
// it, not after.
func (s *Service) SetCAS(store *cas.Store) { s.cas = store }

// CAS returns the content-addressed store, or nil.
func (s *Service) CAS() *cas.Store { return s.cas }

func (s *Service) Store() *Store     { return s.store }
func (s *Service) Blobs() blob.Store { return s.blobs }

// SupportsResumable reports whether the configured blob store can do partial
// writes. The HTTP layer answers 501 rather than 500 when it cannot.
func (s *Service) SupportsResumable() bool { return s.stager != nil }

func (s *Service) stagingStore() blob.Stager { return s.stager }

// Upload streams r into storage and records it as a file.
//
// Ordering is deliberate and load-bearing:
//
//  1. write the bytes, fsync, rename into place
//  2. insert the storage rows, the node and the version in one transaction
//
// A crash between the two leaves unreferenced content, which the GC reclaims.
// Doing it the other way round would leave a file the UI lists, the API
// describes, and nobody can download — a failure that no background job can
// repair because the bytes were never written.
//
// With CAS configured, content at or above cas.WholeFileThreshold goes through
// the chunker; smaller files stay whole-file blobs permanently (a manifest row
// plus a chunk row to describe 900 bytes is pure overhead). The stream's length
// is unknown here, so the router reads exactly one threshold's worth to decide
// — small files fit entirely in that peek, and anything that fills it is big
// enough to chunk.
func (s *Service) Upload(ctx context.Context, ownerID, parentID uuid.UUID, name string, r io.Reader, declaredMIME string) (*Node, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	if s.cas != nil {
		head := make([]byte, cas.WholeFileThreshold)
		n, err := io.ReadFull(r, head)
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			// The whole file fits below the threshold: whole-blob path.
			r = bytes.NewReader(head[:n])
		case err != nil:
			return nil, fmt.Errorf("read upload: %w", err)
		default:
			return s.uploadViaCAS(ctx, ownerID, parentID, name,
				io.MultiReader(bytes.NewReader(head), r), declaredMIME)
		}
	}

	res, err := s.blobs.Put(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("store content: %w", err)
	}

	node, err := s.store.PutFile(ctx, PutFileInput{
		OwnerID:  ownerID,
		ParentID: parentID,
		Name:     name,
		BlobKey:  res.Key,
		Size:     res.Size,
		SHA256:   res.SHA256,
		MIME:     DetectMIME(name, declaredMIME),
	})
	if err != nil {
		// Best effort: reclaim immediately rather than waiting for GC. If this
		// delete also fails the blob is still unreferenced, so the GC remains
		// the backstop and nothing leaks permanently.
		if delErr := s.blobs.Delete(context.WithoutCancel(ctx), res.Key); delErr != nil {
			s.log.Warn("could not remove blob after failed upload",
				"key", res.Key, "error", delErr)
		}
		return nil, err
	}
	return node, nil
}

// uploadViaCAS chunks r and records the file as a manifest-backed version.
func (s *Service) uploadViaCAS(ctx context.Context, ownerID, parentID uuid.UUID, name string, r io.Reader, declaredMIME string) (*Node, error) {
	res, err := s.cas.Write(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("store content: %w", err)
	}

	node, err := s.store.PutFile(ctx, PutFileInput{
		OwnerID:    ownerID,
		ParentID:   parentID,
		Name:       name,
		ManifestID: res.Manifest.ID,
		Size:       res.LogicalSize,
		MIME:       DetectMIME(name, declaredMIME),
	})
	if err != nil {
		s.dropOrphanManifest(ctx, res)
		return nil, err
	}
	return node, nil
}

// dropOrphanManifest is the best-effort cleanup after a failed manifest-backed
// PutFile. Only the manifest row is deleted — NEVER chunk bytes directly, which
// may be shared with other files the moment they exist. The delete re-checks
// that nothing references the manifest, its cascade walks the chunk refcounts
// down, and the chunk GC reclaims whatever lands on zero. If even this fails,
// the manifest is still unreferenced and the GC's manifest pass is the backstop.
func (s *Service) dropOrphanManifest(ctx context.Context, res *cas.WriteResult) {
	if res.ReusedManifest {
		// The manifest belongs to another live file; there is nothing to clean.
		return
	}
	if _, err := s.cas.DeleteManifestRow(context.WithoutCancel(ctx), res.Manifest.ID); err != nil {
		s.log.Warn("could not remove manifest after failed upload",
			"manifest_id", res.Manifest.ID, "error", err)
	}
}

// Open returns the content of a file's head version, reassembling from chunks
// when the version is manifest-backed. Both readers seek, which is what lets
// http.ServeContent answer Range requests either way.
func (s *Service) Open(ctx context.Context, ownerID, id uuid.UUID) (*Node, io.ReadSeekCloser, error) {
	node, err := s.store.GetLive(ctx, ownerID, id)
	if err != nil {
		return nil, nil, err
	}
	if !node.IsFile() {
		return nil, nil, ErrNotAFile
	}

	if node.ManifestID != nil {
		if s.cas == nil {
			// A manifest-backed file with CAS unconfigured is a deployment error,
			// not a data error — the rows and chunks are fine, this process just
			// cannot read them.
			return nil, nil, fmt.Errorf("file %q is content-addressed but CAS is not configured", node.Name)
		}
		rc, err := s.cas.Open(ctx, *node.ManifestID)
		if err != nil {
			return nil, nil, err
		}
		return node, rc, nil
	}

	if node.BlobKey == "" {
		return nil, nil, fmt.Errorf("file %s has no content", node.ID)
	}

	rc, err := s.blobs.Open(ctx, node.BlobKey)
	if errors.Is(err, blob.ErrNotFound) {
		// The row survived but the bytes did not. Loud, because this means the
		// database and the disk have diverged and fsck needs to run.
		s.log.Error("blob missing for existing file",
			"node_id", node.ID, "blob_key", node.BlobKey)
		return nil, nil, fmt.Errorf("content for %q is missing from storage", node.Name)
	}
	if err != nil {
		return nil, nil, err
	}
	return node, rc, nil
}

// FinishStaged turns a completed staging file into a stored file, choosing the
// storage format: CAS chunks at or above cas.WholeFileThreshold, a whole-file
// blob below it. Both the resumable-upload finish and WebDAV's write handle end
// here, so the format decision lives in exactly one place.
//
// sha256 is the whole-file digest accumulated while the bytes arrived; it is
// recorded on the blob path and unused on the CAS path, where the manifest's
// BLAKE3 serves as the content hash.
func (s *Service) FinishStaged(ctx context.Context, ownerID, parentID uuid.UUID, name, stagingKey string, size int64, sha256 []byte, mime string) (*Node, error) {
	if s.stager == nil {
		return nil, errors.New("the configured storage backend does not support staged writes")
	}
	if s.cas != nil && size >= cas.WholeFileThreshold {
		// The staging file lives inside the blob root, so the store can open it
		// by its key directly — no rename, no second copy. It is removed only
		// after the version exists; until then a crash leaves a session the GC
		// can still account for.
		rc, err := s.blobs.Open(ctx, stagingKey)
		if err != nil {
			return nil, fmt.Errorf("open staged upload: %w", err)
		}
		res, err := s.cas.Write(ctx, rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("store content: %w", err)
		}

		node, err := s.store.PutFile(ctx, PutFileInput{
			OwnerID:    ownerID,
			ParentID:   parentID,
			Name:       name,
			ManifestID: res.Manifest.ID,
			Size:       res.LogicalSize,
			MIME:       mime,
		})
		if err != nil {
			s.dropOrphanManifest(ctx, res)
			return nil, err
		}
		if err := s.stagingStore().RemovePartial(stagingKey); err != nil {
			s.log.Warn("could not remove staging file after chunked finish",
				"key", stagingKey, "error", err)
		}
		return node, nil
	}

	blobKey, err := s.stagingStore().CommitPartial(stagingKey)
	if err != nil {
		return nil, err
	}
	node, err := s.store.PutFile(ctx, PutFileInput{
		OwnerID:  ownerID,
		ParentID: parentID,
		Name:     name,
		BlobKey:  blobKey,
		Size:     size,
		SHA256:   sha256,
		MIME:     mime,
	})
	if err != nil {
		if delErr := s.blobs.Delete(context.WithoutCancel(ctx), blobKey); delErr != nil {
			s.log.Warn("could not remove blob after failed staged finish",
				"key", blobKey, "error", delErr)
		}
		return nil, err
	}
	return node, nil
}

// --- garbage collection -----------------------------------------------------

type GCResult struct {
	TrashPurged    int64
	BlobsFreed     int
	BytesFreed     int64
	UploadsExpired int
	StagingFreed   int

	ManifestsFreed  int
	ChunksFreed     int
	ChunkBytesFreed int64
}

// CollectGarbage purges expired trash and then deletes the blobs that fall to
// zero references as a result.
//
// The database row goes first, and only then the file. The reverse would create
// a window in which a row points at a deleted file — the exact state that makes
// a download return a 500 instead of content.
func (s *Service) CollectGarbage(ctx context.Context) (GCResult, error) {
	var res GCResult

	purged, err := s.store.AutoPurgeTrash(ctx, s.TrashRetention)
	if err != nil {
		return res, fmt.Errorf("purge expired trash: %w", err)
	}
	res.TrashPurged = purged

	expired, staged, err := s.collectExpiredUploads(ctx)
	if err != nil {
		return res, err
	}
	res.UploadsExpired, res.StagingFreed = expired, staged

	// Bounded per pass so a large cleanup cannot hold resources for an
	// unbounded time; the next tick picks up where this one stopped.
	candidates, err := s.store.UnreferencedBlobs(ctx, s.BlobGCGrace, 1000)
	if err != nil {
		return res, fmt.Errorf("list unreferenced blobs: %w", err)
	}

	for _, b := range candidates {
		deleted, err := s.store.DeleteBlobRow(ctx, b.ID)
		if err != nil {
			return res, fmt.Errorf("delete blob row: %w", err)
		}
		if !deleted {
			// A new version claimed it between the list and now.
			continue
		}
		if err := s.blobs.Delete(ctx, b.StorageKey); err != nil {
			// The row is gone, so the file is now an orphan on disk rather than
			// a dangling reference — the safe direction. fsck sweeps it.
			s.log.Warn("blob row deleted but file remains", "key", b.StorageKey, "error", err)
			continue
		}
		res.BlobsFreed++
		res.BytesFreed += b.Size
	}

	// Manifests before chunks, deliberately: deleting a manifest is what walks
	// its chunks' refcounts to zero, so this ordering lets a single GC pass
	// reclaim a purged file all the way down to its bytes instead of leaving
	// the chunks for the next run.
	manifestsFreed, err := s.collectManifests(ctx)
	if err != nil {
		return res, err
	}
	res.ManifestsFreed = manifestsFreed

	chunksFreed, chunkBytes, err := s.collectChunks(ctx)
	if err != nil {
		return res, err
	}
	res.ChunksFreed, res.ChunkBytesFreed = chunksFreed, chunkBytes

	return res, nil
}

// collectManifests reaps manifests no file version references any more —
// the residue of purged trash and of failed uploads.
func (s *Service) collectManifests(ctx context.Context) (int, error) {
	if s.cas == nil {
		return 0, nil
	}

	candidates, err := s.cas.UnreferencedManifests(ctx, s.BlobGCGrace, 1000)
	if err != nil {
		return 0, fmt.Errorf("list unreferenced manifests: %w", err)
	}

	var freed int
	for _, id := range candidates {
		// Re-checks inside the delete: manifest reuse means an identical upload
		// can claim this manifest between the list and now, and losing that
		// race must be a no-op rather than severing a fresh file's content.
		deleted, err := s.cas.DeleteManifestRow(ctx, id)
		if err != nil {
			return freed, fmt.Errorf("delete manifest row: %w", err)
		}
		if deleted {
			freed++
		}
	}
	return freed, nil
}

// collectChunks reclaims chunks no manifest references any more.
//
// Row first, then bytes, exactly as for blobs. The reverse would briefly leave
// a chunk row pointing at content already deleted — and because chunks are
// shared, that window would break files belonging to people who had nothing to
// do with whatever triggered the collection.
func (s *Service) collectChunks(ctx context.Context) (int, int64, error) {
	if s.cas == nil {
		return 0, 0, nil
	}

	candidates, err := s.cas.UnreferencedChunks(ctx, s.BlobGCGrace, 1000)
	if err != nil {
		return 0, 0, fmt.Errorf("list unreferenced chunks: %w", err)
	}

	var freed int
	var bytes int64
	for _, c := range candidates {
		// Re-checks refcount inside the delete. Dedup means popular chunks are
		// claimed constantly, so losing that race is routine rather than
		// exotic — and the cost of getting it wrong is another user's file.
		deleted, err := s.cas.DeleteChunkRow(ctx, c.Hash)
		if err != nil {
			return freed, bytes, fmt.Errorf("delete chunk row: %w", err)
		}
		if !deleted {
			continue
		}
		if err := s.blobs.Delete(ctx, c.StorageKey); err != nil {
			// The row is gone, so this is now an orphan on disk rather than a
			// dangling reference — the safe direction. fsck sweeps it.
			s.log.Warn("chunk row deleted but file remains", "key", c.StorageKey, "error", err)
			continue
		}
		freed++
		bytes += int64(c.StoredSize)
	}
	return freed, bytes, nil
}

// collectExpiredUploads removes abandoned resumable uploads, then sweeps
// staging files whose session row is gone.
//
// The staging sweep is driven by the database, not by file age: a file with no
// session row is provably dead, whereas "old" would eventually delete a very
// large upload that a client is still slowly working through.
func (s *Service) collectExpiredUploads(ctx context.Context) (expired, swept int, err error) {
	if s.stager == nil {
		return 0, 0, nil
	}

	sessions, err := s.store.ExpiredUploads(ctx, 500)
	if err != nil {
		return 0, 0, fmt.Errorf("list expired uploads: %w", err)
	}
	for _, sess := range sessions {
		if err := s.store.DeleteUpload(ctx, sess.OwnerID, sess.ID); err != nil {
			s.log.Warn("could not delete expired upload session", "upload_id", sess.ID, "error", err)
			continue
		}
		expired++
	}

	known, err := s.store.AllStagingKeys(ctx)
	if err != nil {
		return expired, 0, fmt.Errorf("list staging keys: %w", err)
	}

	var orphans []string
	if err := s.stager.WalkStaging(func(key string, _ int64) error {
		if !known[key] {
			orphans = append(orphans, key)
		}
		return nil
	}); err != nil {
		return expired, 0, fmt.Errorf("walk staging area: %w", err)
	}
	for _, key := range orphans {
		if err := s.stager.RemovePartial(key); err != nil {
			s.log.Warn("could not remove orphan staging file", "key", key, "error", err)
			continue
		}
		swept++
	}
	return expired, swept, nil
}

// --- fsck -------------------------------------------------------------------

// FsckReport describes disagreements between the database and the disk.
type FsckReport struct {
	// BlobsOnDisk with no matching row. Safe to delete; they are the residue of
	// crashes between writing bytes and committing the transaction.
	Orphans []string
	// OrphanBytes is what deleting them would reclaim.
	OrphanBytes int64

	// Missing rows whose bytes are gone. This is data loss and cannot be fixed
	// by fsck — it can only be reported, so it can be restored from backup.
	Missing []string

	// SizeMismatch rows whose on-disk size disagrees with the recorded size.
	SizeMismatch []string

	// MissingChunks is data loss too, and worse than Missing: a chunk is
	// shared, so its absence damages every file referencing it — potentially
	// files belonging to people who were never involved.
	MissingChunks []string

	// RefcountDrift means the maintenance trigger did not do its job. Reported,
	// never silently corrected.
	RefcountDrift []string

	TempFilesRemoved int
	Checked          int
	ChunksChecked    int
}

// Fsck compares the blob store against the database.
//
// Read-only unless repair is set, and even then it only removes things that are
// provably unreferenced. Nothing here deletes a row: a report that says "these
// bytes are missing" is far more useful than a tool that quietly erases the
// records of files the user still expects to exist.
//
// It MUST account for both storage formats. Whole-file blobs and CAS chunks
// share one blob store root and one directory layout, so a checker that knew
// only about `blobs` would classify every chunk on disk as an orphan — and
// `--repair` would then delete all deduplicated content in the system. That is
// not a hypothetical: it is what this function did before chunks existed.
func (s *Service) Fsck(ctx context.Context, repair bool) (*FsckReport, error) {
	fs, ok := s.blobs.(*blob.FSStore)
	if !ok {
		return nil, errors.New("fsck requires the filesystem blob store")
	}

	known, err := s.store.AllBlobKeys(ctx)
	if err != nil {
		return nil, err
	}

	// Chunk keys, when a CAS store is configured. Absent one, an empty map is
	// correct — there are no chunks to account for.
	chunks := map[string]cas.ChunkRef{}
	if s.cas != nil {
		if chunks, err = s.cas.AllChunkKeys(ctx); err != nil {
			return nil, fmt.Errorf("load chunk keys: %w", err)
		}
	}

	report := &FsckReport{}
	seen := make(map[string]bool, len(known))
	seenChunks := make(map[string]bool, len(chunks))

	err = fs.Walk(func(key string, size int64) error {
		report.Checked++

		if ref, ok := known[key]; ok {
			seen[key] = true
			if ref.Size != size {
				report.SizeMismatch = append(report.SizeMismatch,
					fmt.Sprintf("%s: database says %d bytes, disk has %d", key, ref.Size, size))
			}
			return nil
		}

		if ref, ok := chunks[key]; ok {
			seenChunks[key] = true
			report.ChunksChecked++
			// stored_size, not size: what is on disk is the compressed form.
			if int64(ref.StoredSize) != size {
				report.SizeMismatch = append(report.SizeMismatch,
					fmt.Sprintf("chunk %s: database says %d stored bytes, disk has %d",
						key, ref.StoredSize, size))
			}
			return nil
		}

		report.Orphans = append(report.Orphans, key)
		report.OrphanBytes += size
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk blob store: %w", err)
	}

	for key := range known {
		if !seen[key] {
			report.Missing = append(report.Missing, key)
		}
	}
	for key := range chunks {
		if !seenChunks[key] {
			// A missing chunk is worse than a missing blob: it is shared, so it
			// damages every file that references it, potentially across users.
			report.MissingChunks = append(report.MissingChunks, key)
		}
	}

	// Refcount drift means the trigger did not do its job — a bug, a manual
	// edit, or a half-applied migration. Reported, never silently corrected:
	// an inflated count only wastes disk, but a wrongly deflated one gets live
	// chunks deleted, and "fixing" the number first would destroy the evidence.
	if s.cas != nil {
		drift, err := s.cas.VerifyRefcounts(ctx, 100)
		if err != nil {
			return nil, err
		}
		for _, d := range drift {
			report.RefcountDrift = append(report.RefcountDrift,
				fmt.Sprintf("chunk %x: recorded %d references, actually %d",
					d.Hash, d.Recorded, d.Actual))
		}
	}

	if repair {
		n, err := fs.SweepTempFiles()
		if err != nil {
			return report, fmt.Errorf("sweep temp files: %w", err)
		}
		report.TempFilesRemoved = n

		for _, key := range report.Orphans {
			if err := s.blobs.Delete(ctx, key); err != nil {
				s.log.Warn("could not delete orphan blob", "key", key, "error", err)
			}
		}
	}
	return report, nil
}

// --- mime -------------------------------------------------------------------

// builtinTypes is an explicit extension table.
//
// Go's mime.TypeByExtension reads /etc/mime.types, and the distroless runtime
// image does not have one — so relying on it alone gives correct types in
// development and "application/octet-stream" for everything in production, the
// worst possible split. Its own compiled-in table covers barely a dozen web
// formats and misses .txt entirely. This table is consulted first so behaviour
// is identical everywhere, regardless of what the base image ships.
var builtinTypes = map[string]string{
	".txt": "text/plain; charset=utf-8", ".md": "text/markdown; charset=utf-8",
	".csv": "text/csv; charset=utf-8", ".log": "text/plain; charset=utf-8",
	".json": "application/json", ".xml": "application/xml",
	".yaml": "application/yaml", ".yml": "application/yaml",
	".pdf": "application/pdf", ".rtf": "application/rtf",
	".zip": "application/zip", ".gz": "application/gzip",
	".tar": "application/x-tar", ".7z": "application/x-7z-compressed",

	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".avif": "image/avif",
	".bmp": "image/bmp", ".ico": "image/x-icon", ".heic": "image/heic",
	// SVG is served with the sandboxing headers the download handler sets;
	// without those this type would be a stored-XSS vector.
	".svg": "image/svg+xml", ".tif": "image/tiff", ".tiff": "image/tiff",

	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".flac": "audio/flac",
	".wav": "audio/wav", ".ogg": "audio/ogg", ".opus": "audio/opus",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime",
	".webm": "video/webm", ".mkv": "video/x-matroska", ".avi": "video/x-msvideo",

	".doc": "application/msword", ".xls": "application/vnd.ms-excel",
	".ppt":  "application/vnd.ms-powerpoint",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".epub": "application/epub+zip",
}

// DetectMIME picks a content type from the filename, falling back to what the
// client claimed.
//
// The extension wins over the client's header on purpose. Browsers and CLI
// tools send "application/octet-stream" for anything they do not recognise, and
// trusting that would make every download an attachment even when the server
// can see it is a PNG.
func DetectMIME(name, declared string) string {
	if ext := strings.ToLower(filepath.Ext(name)); ext != "" {
		if t, ok := builtinTypes[ext]; ok {
			return t
		}
		if byExt := mime.TypeByExtension(ext); byExt != "" {
			return byExt
		}
	}
	declared = strings.TrimSpace(declared)
	if declared == "" || declared == "application/octet-stream" {
		return "application/octet-stream"
	}
	// Strip parameters ("text/plain; charset=utf-8" -> the full string is fine,
	// but a malformed one is not worth storing).
	if _, _, err := mime.ParseMediaType(declared); err != nil {
		return "application/octet-stream"
	}
	return declared
}
