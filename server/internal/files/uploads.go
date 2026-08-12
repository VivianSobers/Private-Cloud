package files

import (
	"context"
	"crypto/sha256"
	"encoding"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrUploadNotFound = errors.New("upload session not found or expired")
	ErrOffsetMismatch = errors.New("upload offset does not match")
	ErrUploadLocked   = errors.New("another request is writing to this upload")
	ErrUploadTooLarge = errors.New("upload exceeds the declared length")
)

// UploadSession is an in-progress resumable upload.
type UploadSession struct {
	ID       uuid.UUID
	OwnerID  uuid.UUID
	ParentID uuid.UUID
	Name     string
	MIME     string

	Size   int64 // declared total
	Offset int64 // bytes durably written

	StagingKey string
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

func (u *UploadSession) Complete() bool { return u.Offset >= u.Size }

// lockWindow is how long a PATCH may hold an upload before another request is
// allowed to assume the writer died. Refreshed while a chunk is in flight, so
// the window bounds "how long a dead client wedges its own upload", not "how
// long a chunk may take".
const lockWindow = 2 * time.Minute

// --- store ------------------------------------------------------------------

func (s *Store) CreateUpload(ctx context.Context, u *UploadSession, ttl time.Duration) (*UploadSession, error) {
	// The parent must be a live folder the caller may WRITE to — which is not the
	// same as one they own. An editor may upload into a folder shared with them,
	// and Phase 7 made that true of POST /upload without making it true here, so
	// the browser's own 8 MiB threshold decided whether an editor's upload
	// worked. WriteOwnerFor is the single answer to "may this caller write here",
	// and it returns ErrNotFound for read-only access exactly as for no access.
	//
	// Checked at create rather than at the first PATCH so an upload into a folder
	// that is gone, or was never writable, fails before the client has spent an
	// hour transferring.
	//
	// The SESSION stays the caller's — u.OwnerID is untouched — because the caller
	// is who resumes it, cancels it and holds the staging file. Which tree the
	// finished FILE lands in is a different question, resolved once at finish by
	// FinishUpload.
	if _, err := s.WriteOwnerFor(ctx, u.OwnerID, u.ParentID); err != nil {
		return nil, err
	}

	var kind string
	err := s.pool.QueryRow(ctx, `
		SELECT kind FROM nodes WHERE id = $1 AND trashed_at IS NULL`,
		u.ParentID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if kind != KindFolder {
		return nil, ErrNotAFolder
	}

	err = s.pool.QueryRow(ctx, `
		INSERT INTO upload_sessions
			(owner_id, parent_id, name, mime, size, staging_key, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6, now() + $7::interval)
		RETURNING id, upload_offset, expires_at, created_at`,
		u.OwnerID, u.ParentID, u.Name, u.MIME, u.Size, u.StagingKey, ttl.String(),
	).Scan(&u.ID, &u.Offset, &u.ExpiresAt, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create upload session: %w", err)
	}
	return u, nil
}

func (s *Store) GetUpload(ctx context.Context, ownerID, id uuid.UUID) (*UploadSession, error) {
	u := &UploadSession{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, owner_id, parent_id, name, mime, size, upload_offset,
		       staging_key, expires_at, created_at
		FROM upload_sessions
		WHERE id = $1 AND owner_id = $2 AND expires_at > now()`, id, ownerID,
	).Scan(&u.ID, &u.OwnerID, &u.ParentID, &u.Name, &u.MIME, &u.Size, &u.Offset,
		&u.StagingKey, &u.ExpiresAt, &u.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUploadNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// LockUpload claims an upload for writing and returns its committed state.
//
// A conditional UPDATE rather than SELECT ... FOR UPDATE: a row lock would have
// to be held inside a transaction for the entire duration of the chunk
// transfer, tying up a pool connection for however long the client's network
// takes. A timestamp column costs one statement and releases itself if the
// writer dies.
func (s *Store) LockUpload(ctx context.Context, ownerID, id uuid.UUID) (*UploadSession, hash.Hash, error) {
	u := &UploadSession{}
	var state []byte

	err := s.pool.QueryRow(ctx, `
		UPDATE upload_sessions
		SET locked_until = now() + $3::interval
		WHERE id = $1 AND owner_id = $2 AND expires_at > now()
		  AND (locked_until IS NULL OR locked_until < now())
		RETURNING id, owner_id, parent_id, name, mime, size, upload_offset,
		          staging_key, hash_state, expires_at, created_at`,
		id, ownerID, lockWindow.String(),
	).Scan(&u.ID, &u.OwnerID, &u.ParentID, &u.Name, &u.MIME, &u.Size, &u.Offset,
		&u.StagingKey, &state, &u.ExpiresAt, &u.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		// Either it does not exist, or someone else holds the lock. Distinguish
		// them so the client gets 404 vs 423 rather than a confusing single
		// answer for two very different situations.
		if _, getErr := s.GetUpload(ctx, ownerID, id); getErr == nil {
			return nil, nil, ErrUploadLocked
		}
		return nil, nil, ErrUploadNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	hasher, err := restoreHash(state)
	if err != nil {
		return nil, nil, err
	}
	return u, hasher, nil
}

// RefreshUploadLock extends the lock while a chunk is still arriving.
func (s *Store) RefreshUploadLock(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE upload_sessions SET locked_until = now() + $2::interval WHERE id = $1`,
		id, lockWindow.String())
	return err
}

// CommitUploadProgress records durable progress and releases the lock.
//
// The offset and the hash state are written in ONE statement. Split across two,
// a crash between them would leave a hash that does not describe the bytes the
// offset claims — and the mismatch would only surface at the end of a
// multi-gigabyte upload, with no way to tell which half was wrong.
func (s *Store) CommitUploadProgress(ctx context.Context, id uuid.UUID, offset int64, hasher hash.Hash) error {
	state, err := marshalHash(hasher)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE upload_sessions
		SET upload_offset = $2, hash_state = $3, locked_until = NULL, updated_at = now()
		WHERE id = $1`, id, offset, state)
	return err
}

// UnlockUpload releases the lock without recording progress.
func (s *Store) UnlockUpload(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE upload_sessions SET locked_until = NULL WHERE id = $1`, id)
	return err
}

func (s *Store) DeleteUpload(ctx context.Context, ownerID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM upload_sessions WHERE id = $1 AND owner_id = $2`, id, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUploadNotFound
	}
	return nil
}

// ExpiredUploads lists sessions past their deadline, so their staging files can
// be removed before the rows are.
func (s *Store) ExpiredUploads(ctx context.Context, limit int) ([]UploadSession, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_id, staging_key FROM upload_sessions
		WHERE expires_at < now() ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UploadSession
	for rows.Next() {
		var u UploadSession
		if err := rows.Scan(&u.ID, &u.OwnerID, &u.StagingKey); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// AllStagingKeys returns every staging key the database still expects to exist.
func (s *Store) AllStagingKeys(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT staging_key FROM upload_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// --- hash state -------------------------------------------------------------
//
// crypto/sha256's digest implements encoding.BinaryMarshaler exactly so a hash
// can be suspended and resumed. That is what lets the content hash be computed
// incrementally across many requests instead of re-reading a finished
// multi-gigabyte file just to learn its digest.

func restoreHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	if len(state) == 0 {
		return h, nil
	}
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, errors.New("sha256 does not support resumable state")
	}
	if err := u.UnmarshalBinary(state); err != nil {
		return nil, fmt.Errorf("restore hash state: %w", err)
	}
	return h, nil
}

func marshalHash(h hash.Hash) ([]byte, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, errors.New("sha256 does not support resumable state")
	}
	return m.MarshalBinary()
}

// --- service ----------------------------------------------------------------

// UploadTTL bounds how long an abandoned resumable upload occupies disk.
const defaultUploadTTL = 24 * time.Hour

// CreateUpload starts a resumable upload.
func (s *Service) CreateUpload(ctx context.Context, ownerID, parentID uuid.UUID, name string, size int64, declaredMIME string) (*UploadSession, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, ErrUploadTooLarge
	}

	// Check quota before accepting a single byte. Discovering at 99% that the
	// file will not fit is the worst possible moment to find out, and it is
	// entirely avoidable when the client declares the length up front.
	if err := s.checkQuotaFor(ctx, ownerID, size); err != nil {
		return nil, err
	}

	key, err := s.stagingStore().CreatePartial()
	if err != nil {
		return nil, err
	}

	sess, err := s.store.CreateUpload(ctx, &UploadSession{
		OwnerID: ownerID, ParentID: parentID, Name: name,
		MIME: DetectMIME(name, declaredMIME), Size: size, StagingKey: key,
	}, s.UploadTTL)
	if err != nil {
		// The row never existed, so nothing will ever reclaim this file.
		if rmErr := s.stagingStore().RemovePartial(key); rmErr != nil {
			s.log.Warn("could not remove staging file after failed session create",
				"key", key, "error", rmErr)
		}
		return nil, err
	}
	return sess, nil
}

// AppendChunk writes the next piece of a resumable upload.
//
// offset is the client's claim about where it is resuming. It must match the
// server's committed offset exactly: tus mandates 409 otherwise, and accepting
// a mismatch would either duplicate or skip a range of the file.
func (s *Service) AppendChunk(ctx context.Context, ownerID, id uuid.UUID, offset int64, r io.Reader) (*UploadSession, error) {
	sess, hasher, err := s.store.LockUpload(ctx, ownerID, id)
	if err != nil {
		return nil, err
	}

	if offset != sess.Offset {
		_ = s.store.UnlockUpload(ctx, id)
		return nil, fmt.Errorf("%w: server is at %d, client sent %d", ErrOffsetMismatch, sess.Offset, offset)
	}

	// Keep the lock alive while the chunk transfers. Without this a slow client
	// would lose its own lock mid-write and a retry could start a second
	// concurrent writer against the same file.
	refreshCtx, stopRefresh := context.WithCancel(ctx)
	go s.refreshLock(refreshCtx, id)

	// Never accept more than was declared. A client that keeps sending would
	// otherwise fill the disk regardless of the quota checked at creation.
	remaining := sess.Size - sess.Offset
	limited := io.LimitReader(r, remaining+1)

	written, copyErr := s.stagingStore().AppendPartial(ctx, sess.StagingKey, sess.Offset, hasher, limited)
	stopRefresh()

	if written > remaining {
		// The hash and the file now both contain a byte too many. Discarding
		// the whole session is the only honest response: there is no way to
		// un-hash the excess.
		_ = s.store.UnlockUpload(ctx, id)
		return nil, ErrUploadTooLarge
	}

	newOffset := sess.Offset + written

	// Commit whatever arrived, even if the copy failed. The bytes are fsynced
	// and hashed; refusing to record them would make every network blip cost
	// the client the entire chunk it had already delivered.
	if err := s.store.CommitUploadProgress(ctx, id, newOffset, hasher); err != nil {
		return nil, err
	}
	if copyErr != nil && !errors.Is(copyErr, io.ErrUnexpectedEOF) {
		// The client hung up mid-chunk. Progress is saved; it can resume.
		s.log.Debug("resumable upload chunk interrupted",
			"upload_id", id, "offset", newOffset, "error", copyErr)
	}

	sess.Offset = newOffset
	return sess, nil
}

func (s *Service) refreshLock(ctx context.Context, id uuid.UUID) {
	ticker := time.NewTicker(lockWindow / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// context.WithoutCancel: the refresh must still run when the parent
			// is a request context that is about to be cancelled.
			if err := s.store.RefreshUploadLock(context.WithoutCancel(ctx), id); err != nil {
				return
			}
		}
	}
}

// FinishUpload turns a completed session into a file.
func (s *Service) FinishUpload(ctx context.Context, ownerID, id uuid.UUID) (*Node, error) {
	sess, hasher, err := s.store.LockUpload(ctx, ownerID, id)
	if err != nil {
		return nil, err
	}
	if !sess.Complete() {
		_ = s.store.UnlockUpload(ctx, id)
		return nil, fmt.Errorf("%w: %d of %d bytes received", ErrOffsetMismatch, sess.Offset, sess.Size)
	}

	// Verify the file on disk is the length the session claims. This is the one
	// place the two sources of truth can be compared, and a mismatch means the
	// staging file was tampered with or the filesystem lost a write.
	onDisk, err := s.stagingStore().StatPartial(sess.StagingKey)
	if err != nil {
		return nil, fmt.Errorf("stat staged upload: %w", err)
	}
	if onDisk != sess.Size {
		return nil, fmt.Errorf("staged upload is %d bytes, expected %d", onDisk, sess.Size)
	}

	// Whose tree the finished file lands in, and whose quota it spends. The
	// session belongs to whoever is uploading; the FILE belongs to the folder's
	// owner, which for an ordinary upload into your own tree is the same person
	// and for an editor writing into a shared folder is not. Resolved here rather
	// than carried on the session so a grant revoked mid-upload takes effect.
	fileOwner, err := s.store.WriteOwnerFor(ctx, sess.OwnerID, sess.ParentID)
	if err != nil {
		return nil, err
	}

	// FinishStaged owns the storage-format decision (CAS chunks vs whole-file
	// blob) and the cleanup on failure, for this path and WebDAV's alike.
	node, err := s.FinishStaged(ctx, fileOwner, sess.ParentID, sess.Name,
		sess.StagingKey, sess.Size, hasher.Sum(nil), sess.MIME)
	if err != nil {
		return nil, err
	}

	// The staging file is gone (renamed or chunked away), so deleting the row
	// cannot orphan anything. Done after the node exists so a failure here is
	// merely a stale session the GC cleans up, not a lost file.
	if err := s.store.DeleteUpload(ctx, ownerID, id); err != nil {
		s.log.Warn("could not delete finished upload session", "upload_id", id, "error", err)
	}
	return node, nil
}

// CancelUpload discards an in-progress upload and its bytes.
func (s *Service) CancelUpload(ctx context.Context, ownerID, id uuid.UUID) error {
	sess, err := s.store.GetUpload(ctx, ownerID, id)
	if err != nil {
		return err
	}
	if err := s.store.DeleteUpload(ctx, ownerID, id); err != nil {
		return err
	}
	// Row first, then bytes: the reverse would briefly leave a session whose
	// staging file is already gone, and a concurrent PATCH would fail with a
	// confusing "not found" from the filesystem instead of a clean 404.
	if err := s.stagingStore().RemovePartial(sess.StagingKey); err != nil {
		s.log.Warn("could not remove staging file", "key", sess.StagingKey, "error", err)
	}
	return nil
}

func (s *Service) checkQuotaFor(ctx context.Context, ownerID uuid.UUID, adding int64) error {
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	return checkQuota(ctx, tx, ownerID, adding)
}
