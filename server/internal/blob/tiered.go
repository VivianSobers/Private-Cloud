package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"sync"
	"time"
)

// ErrRestoring means the content is in the cold tier and is being fetched back.
//
// It is a distinct error, not a flavour of ErrNotFound, because the two demand
// opposite responses. ErrNotFound on a key the database knows about is data
// loss and sends an operator to a backup; this means the bytes are exactly
// where they are supposed to be and the caller should ask again shortly. The
// HTTP layer turns it into 202 restore_in_progress with a Retry-After, which is
// the contract docs/api-contract.md specified for the download path before any
// of this existed.
var ErrRestoring = errors.New("blob is in the cold tier and is being restored")

// TieredStore composes a local (hot) store with an object-storage (cold) one.
//
// It is a blob.Store, so nothing above it changes: files.Service, cas.Store and
// every handler keep calling Open/Put/Stat/Delete and are never told which tier
// answered. That transparency is the whole point of the interface being four
// methods wide — the tiering slice adds a second implementation behind it
// rather than a second code path through everything above it.
//
// Reads are served from hot. A miss falls through to a PROMOTION: the object is
// copied back to local disk and served from there, so the Range seeks that
// video scrubbing depends on stay local and the second read costs nothing. A
// promotion that does not finish within RestoreWait keeps running in the
// background and the read returns ErrRestoring — never a block without end, and
// never a 500 for content that is not actually lost.
//
// Writes always go to hot. A tiered store that wrote new content straight to
// object storage would make every upload as slow as the uplink, and there is no
// case for it: content becomes cold by being old, which nothing being written
// is.
type TieredStore struct {
	hot  *FSStore
	cold Store
	// coldKeyed is the same store, when it can address content by key. A cold
	// tier that cannot is readable but not writable by this system: demotion has
	// to preserve the key, because every row in the database points at it.
	coldKeyed KeyedPutter
	log       *slog.Logger

	// RestoreWait bounds how long a read will wait for a promotion before
	// answering "come back". It exists so a cold read has a stated latency
	// contract rather than an unbounded one: a request that hangs for eight
	// minutes on a cold multi-gigabyte video has already failed, it just has not
	// said so yet.
	RestoreWait time.Duration

	// recorder, when set, keeps the database's tier column and access times in
	// step with what this store actually did. Optional, and deliberately so:
	// blob knows nothing about `blobs` or `chunks`, and a test store should not
	// need a database to prove the promotion works.
	recorder TierRecorder

	// inflight de-duplicates concurrent promotions of the same key. Ten viewers
	// opening the same cold video must pull it back once, not ten times, and
	// the ten copies would race to rename over each other.
	mu       sync.Mutex
	inflight map[string]*restore
}

// TierRecorder is how the storage layer tells the database what moved. Kept as
// an interface here so this package does not import the schema; the wiring in
// cmd/ supplies an implementation backed by the tier column.
type TierRecorder interface {
	// MarkHot records that a key's bytes are on local disk again. Called after a
	// promotion completes, so the demotion policy and fsck agree with reality.
	MarkHot(ctx context.Context, key string) error
	// Touch records that a key was read, for the access-recency half of the
	// demotion policy. Best effort and rate-limited by the implementation: this
	// is on the download path, and a database write per range request would cost
	// more than the tiering saves.
	Touch(ctx context.Context, key string)
}

// restore is one in-flight promotion. done closes when it finishes; err holds
// the outcome for whoever is still waiting.
type restore struct {
	done chan struct{}
	err  error
}

// NewTieredStore wraps a hot store with a cold one. A nil cold store returns
// the hot store's behaviour unchanged, which is what makes the cold tier a
// switch an operator can turn off without the rest of the system noticing.
func NewTieredStore(hot *FSStore, cold Store, log *slog.Logger) *TieredStore {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	t := &TieredStore{
		hot:         hot,
		cold:        cold,
		log:         log,
		RestoreWait: 10 * time.Second,
		inflight:    map[string]*restore{},
	}
	if kp, ok := cold.(KeyedPutter); ok {
		t.coldKeyed = kp
	}
	return t
}

// SetRecorder wires the database side of tiering. Called at startup; left unset
// in tests and in processes that only read.
func (t *TieredStore) SetRecorder(r TierRecorder) { t.recorder = r }

// Hot exposes the local store. fsck needs it: the local walk is what finds
// orphans, and it must be the hot root that is walked, never the bucket.
func (t *TieredStore) Hot() *FSStore { return t.hot }

// Cold exposes the object store, or nil. fsck needs this too — to confirm that
// a row marked cold really does have its bytes somewhere before it decides
// anything about them.
func (t *TieredStore) Cold() Store { return t.cold }

// Enabled reports whether a cold tier is configured at all.
func (t *TieredStore) Enabled() bool { return t.cold != nil }

// --- Store --------------------------------------------------------------

// Put writes to the hot tier. New content is never cold; see the type comment.
func (t *TieredStore) Put(ctx context.Context, r io.Reader) (*PutResult, error) {
	return t.hot.Put(ctx, r)
}

// PutKeyed writes to the hot tier, but reports a key already present in EITHER
// tier as existing.
//
// That second half is load-bearing. The key is the content hash, so an object
// sitting in the cold tier already holds byte for byte what would be written —
// and re-writing it hot would silently un-demote content the policy job had
// deliberately moved, on nothing more than someone re-uploading a file they
// share with the original owner. Dedup's promise is that identical content is
// stored once; it does not say which tier.
func (t *TieredStore) PutKeyed(ctx context.Context, key string, r io.Reader) (bool, error) {
	if t.cold != nil {
		if _, err := t.hot.Stat(ctx, key); errors.Is(err, ErrNotFound) {
			if _, err := t.cold.Stat(ctx, key); err == nil {
				// Drain the reader so a caller that streams from a network
				// connection is not left with a half-read body.
				_, _ = io.Copy(io.Discard, r)
				return true, nil
			} else if !errors.Is(err, ErrNotFound) {
				return false, err
			}
		}
	}
	return t.hot.PutKeyed(ctx, key, r)
}

// Open serves from hot, promoting from cold on a miss.
func (t *TieredStore) Open(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	rc, err := t.hot.Open(ctx, key)
	if err == nil {
		t.touch(ctx, key)
		return rc, nil
	}
	if !errors.Is(err, ErrNotFound) || t.cold == nil {
		return nil, err
	}

	if err := t.Warm(ctx, key); err != nil {
		return nil, err
	}
	// The promotion reported success, so the bytes are local now. A NotFound
	// here would mean something deleted them between the rename and this open,
	// which is a real fault and is reported as one rather than looping.
	rc, err = t.hot.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	t.touch(ctx, key)
	return rc, nil
}

// Warm ensures a key's bytes are on local disk, returning ErrRestoring if that
// did not finish within RestoreWait.
//
// Exported because the read path needs it one level up as well: a
// manifest-backed file is reassembled from many chunks, and discovering the
// third of them is cold halfway through writing a 200 response is too late to
// say anything useful. cas asks this first, for every cold chunk of a manifest,
// so the answer arrives before any bytes do.
func (t *TieredStore) Warm(ctx context.Context, key string) error {
	if t.cold == nil {
		return ErrNotFound
	}
	if _, err := t.hot.Stat(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	r := t.begin(key)

	wait := t.RestoreWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-r.done:
		return r.err
	case <-timer.C:
		// Still going. The goroutine keeps the copy running to completion — a
		// restore abandoned at 90% and restarted on the next request would never
		// finish for a file large enough to need this path in the first place.
		return fmt.Errorf("%w: %s", ErrRestoring, key)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StartRestore begins a promotion without waiting for it.
//
// The reason it is separate from Warm is a chunked file: reassembling one may
// need twenty cold chunks, and warming them one at a time would make the client
// wait RestoreWait for each in turn. Starting all of them and only then waiting
// turns twenty serial transfers into twenty concurrent ones.
func (t *TieredStore) StartRestore(key string) {
	if t.cold == nil {
		return
	}
	if _, err := t.hot.Stat(context.Background(), key); err == nil {
		return
	}
	t.begin(key)
}

// Restoring reports whether a promotion for this key is currently running. The
// admin report uses it; it is not a synchronisation primitive.
func (t *TieredStore) Restoring(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.inflight[key]
	return ok
}

// begin returns the in-flight restore for a key, starting one if there is none.
func (t *TieredStore) begin(key string) *restore {
	t.mu.Lock()
	if r, ok := t.inflight[key]; ok {
		t.mu.Unlock()
		return r
	}
	r := &restore{done: make(chan struct{})}
	t.inflight[key] = r
	t.mu.Unlock()

	go func() {
		// Deliberately NOT the caller's context. The request that triggered the
		// restore is the one most likely to be cancelled — the user gave up and
		// hit reload — and cancelling the copy with it means the next attempt
		// starts from zero, forever.
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()

		r.err = t.promote(ctx, key)

		t.mu.Lock()
		delete(t.inflight, key)
		t.mu.Unlock()
		close(r.done)
	}()
	return r
}

// promote copies one object from cold to hot.
//
// PutKeyed on the hot store, not Put: the key must survive the move, because
// every row in the database points at it. PutKeyed is also the idempotent one,
// so two promotions that somehow overlap converge instead of corrupting.
func (t *TieredStore) promote(ctx context.Context, key string) error {
	src, err := t.cold.Open(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Not on disk and not in the bucket. This is the genuine data-loss
			// case and must not be dressed up as a restore in progress, or the
			// client retries a 202 forever against content that is gone.
			t.log.Error("content is in neither tier", "key", key)
			return ErrNotFound
		}
		return err
	}
	defer src.Close()

	if _, err := t.hot.PutKeyed(ctx, key, src); err != nil {
		return fmt.Errorf("promote %s: %w", key, err)
	}

	// The bytes are local; tell the database so the policy job stops treating
	// this as cold content and fsck stops expecting it in the bucket only. The
	// COLD COPY IS LEFT IN PLACE on purpose: deleting it would make a promotion
	// a move, and a move is the one operation that can lose content if the local
	// copy is subsequently lost. Re-demoting later is a no-op upload, because
	// PutKeyed finds the object already there.
	if t.recorder != nil {
		if err := t.recorder.MarkHot(ctx, key); err != nil {
			// Not fatal to the read: the bytes are local and serving them is
			// correct. The row being stale costs one redundant promotion later.
			t.log.Warn("could not record promotion", "key", key, "error", err)
		}
	}
	t.log.Info("promoted from the cold tier", "key", key)
	return nil
}

// Stat reports the size from whichever tier holds the object.
func (t *TieredStore) Stat(ctx context.Context, key string) (int64, error) {
	n, err := t.hot.Stat(ctx, key)
	if err == nil || !errors.Is(err, ErrNotFound) || t.cold == nil {
		return n, err
	}
	return t.cold.Stat(ctx, key)
}

// Delete removes an object from BOTH tiers.
//
// Deleting from one only is how a cold tier fills with content nothing
// references and nobody can account for. It is safe to delete from both because
// the only caller is the GC, which has a zero refcount and a grace period to
// prove nothing points at these bytes.
func (t *TieredStore) Delete(ctx context.Context, key string) error {
	if err := t.hot.Delete(ctx, key); err != nil {
		return err
	}
	if t.cold == nil {
		return nil
	}
	return t.cold.Delete(ctx, key)
}

func (t *TieredStore) touch(ctx context.Context, key string) {
	if t.recorder == nil {
		return
	}
	t.recorder.Touch(ctx, key)
}

// --- Stager -------------------------------------------------------------

// Resumable uploads run entirely against the hot tier. An upload in progress is
// the newest content in the system by definition, so there is nothing for the
// cold tier to do with it, and staging in object storage would make every
// resumed chunk a network round trip.

func (t *TieredStore) CreatePartial() (string, error) { return t.hot.CreatePartial() }

func (t *TieredStore) AppendPartial(ctx context.Context, key string, offset int64, hasher hash.Hash, r io.Reader) (int64, error) {
	return t.hot.AppendPartial(ctx, key, offset, hasher, r)
}

func (t *TieredStore) CommitPartial(key string) (string, error) { return t.hot.CommitPartial(key) }
func (t *TieredStore) StatPartial(key string) (int64, error)    { return t.hot.StatPartial(key) }
func (t *TieredStore) RemovePartial(key string) error           { return t.hot.RemovePartial(key) }

func (t *TieredStore) WalkStaging(fn func(key string, size int64) error) error {
	return t.hot.WalkStaging(fn)
}

// --- maintenance --------------------------------------------------------

// Walk visits the HOT tier only, which is what fsck's orphan detection wants:
// an orphan is a local file no row references. The cold tier is walked
// separately and to a different end — see Fsck — because a bucket object with
// no local row is not evidence of anything and must never be deleted on that
// basis.
func (t *TieredStore) Walk(fn func(key string, size int64) error) error { return t.hot.Walk(fn) }

// SweepTempFiles sweeps the hot tier. The cold tier has no temp files: an
// object PUT is atomic, so there is no half-written state to leave behind.
func (t *TieredStore) SweepTempFiles() (int, error) { return t.hot.SweepTempFiles() }

// Demote copies one key to the cold tier and, once it has PROVED the copy reads
// back correctly, removes the local one.
//
// The order and the proof are the whole of this function. Content "moved to
// cold" by code that cannot read it back is content that is gone, silently, for
// the files least recently touched — so the local copy is deleted last, after a
// full read-back whose digest is compared with what was uploaded. Any failure
// anywhere leaves the local copy exactly where it was, and the worst outcome is
// a redundant object in the bucket that fsck will report and nothing will
// delete.
//
// The local bytes are digested first, the object is uploaded, and the object is
// then READ BACK and digested again. That is one extra pass over the network per
// demotion, and it is the cheapest possible insurance against a truncated PUT, a
// proxy that ate the body, or a bucket whose prefix points somewhere else
// entirely. It returns the number of bytes moved.
func (t *TieredStore) Demote(ctx context.Context, key string) (int64, error) {
	if t.cold == nil {
		return 0, errors.New("no cold tier is configured")
	}

	want, size, err := t.digestLocal(ctx, key)
	if err != nil {
		return 0, err
	}

	src, err := t.hot.Open(ctx, key)
	if err != nil {
		return 0, err
	}
	_, putErr := t.coldKeyed.PutKeyed(ctx, key, src)
	src.Close()
	if putErr != nil {
		return 0, fmt.Errorf("demote %s: %w", key, putErr)
	}

	back, err := t.cold.Open(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("demote %s: cannot read back what was just written: %w", key, err)
	}
	got := sha256.New()
	n, err := io.Copy(got, back)
	back.Close()
	if err != nil {
		return 0, fmt.Errorf("demote %s: cannot read back what was just written: %w", key, err)
	}
	if n != size || !bytes.Equal(got.Sum(nil), want) {
		// PutKeyed is idempotent on the key being the content hash, so an
		// existing object that disagrees means the bucket holds something else
		// under this name. Refusing is the only safe move: overwriting could
		// destroy another deployment's content, and deleting the local copy
		// would destroy this one's.
		return 0, fmt.Errorf("demote %s: the cold copy does not match the local bytes (%d vs %d bytes)", key, n, size)
	}

	// Only now. Everything above this line is reversible.
	if err := t.hot.Delete(ctx, key); err != nil {
		return 0, fmt.Errorf("demote %s: cold copy is good but the local one could not be removed: %w", key, err)
	}
	return size, nil
}

// digestLocal reads the local copy and returns its SHA-256 and length. A second
// full read of the file is the price of proving the round trip; a demotion runs
// in the background and is not the thing to optimise for latency.
func (t *TieredStore) digestLocal(ctx context.Context, key string) ([]byte, int64, error) {
	rc, err := t.hot.Open(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	defer rc.Close()

	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}
