package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Store is the cold half of the tiered blob store: the same four-method Store
// contract, backed by an S3-compatible object API.
//
// WHY THE STANDARD LIBRARY AND NOT THE AWS SDK. This package needs exactly six
// operations — GET, ranged GET, HEAD, PUT, DELETE and a paged LIST — against a
// service whose signing algorithm is a published, stable specification. The
// official SDK brings in credential-provider chains, IMDS probing, retry
// middleware and a region-resolution layer, none of which apply to a bucket
// whose endpoint and static keys are in the operator's .env. It is the same
// judgement this repository already made when it hand-rolled a Prometheus
// textfile parser rather than take a dependency to read four metric names: the
// cost of the dependency is not the download, it is that every one of those
// layers is a behaviour nobody here has read. SigV4 below is ninety lines and
// its correctness is proved by the fake S3 in s3_test.go, which verifies the
// signature the way a real server does.
//
// PATH-STYLE ADDRESSING, always. Virtual-host style (bucket.endpoint) needs
// wildcard DNS and TLS certificates, which self-hosted MinIO and Garage — the
// realistic cold tiers for this system — do not have by default. AWS itself
// still accepts path style for existing buckets, and an operator pointing at
// real S3 can set the endpoint to the regional host.
type S3Store struct {
	// endpoint is the scheme+host of the service, with no trailing slash.
	endpoint string
	bucket   string
	region   string
	keyID    string
	secret   string

	// prefix is prepended to every key, so one bucket can hold the cold tiers of
	// more than one deployment without them colliding. Empty by default.
	prefix string

	client *http.Client

	// now is overridable so a test can pin the signing timestamp. Production
	// always uses time.Now.
	now func() time.Time
}

// S3Config is what an operator sets. Every field is required except Prefix and
// the client, which is why validation refuses rather than defaulting: a cold
// tier pointed at the wrong bucket is not a condition to discover later.
type S3Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	Prefix    string

	// HTTPClient lets a caller supply timeouts and, in tests, an in-process
	// transport. Nil gets a client with a generous timeout: a cold-tier PUT can
	// legitimately be a multi-gigabyte body over a domestic uplink.
	HTTPClient *http.Client
}

// NewS3Store validates the configuration and returns a store. It does NOT probe
// the bucket: a cold tier that is unreachable at boot must not stop the API
// serving hot content, which is the overwhelming majority of every request. The
// first demotion reports the problem, and demotion is a background job that
// retries.
func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("cold tier endpoint is empty")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("cold tier endpoint %q must be an http:// or https:// URL", cfg.Endpoint)
	}
	if cfg.Bucket == "" {
		return nil, errors.New("cold tier bucket is empty")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("cold tier credentials are incomplete")
	}
	if cfg.Region == "" {
		// Not defaulted silently elsewhere: the region is part of the signature's
		// scope, so a wrong one fails every request with an opaque 403. Saying
		// what was assumed is cheaper than debugging that.
		cfg.Region = "us-east-1"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	return &S3Store{
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		bucket:   cfg.Bucket,
		region:   cfg.Region,
		keyID:    cfg.AccessKey,
		secret:   cfg.SecretKey,
		prefix:   strings.Trim(cfg.Prefix, "/"),
		client:   client,
		now:      time.Now,
	}, nil
}

// Describe reports where this store points, for the startup log and the admin
// storage report. The secret is never part of it.
func (s *S3Store) Describe() (endpoint, bucket, region, prefix string) {
	return s.endpoint, s.bucket, s.region, s.prefix
}

// objectPath maps a store key to the bucket-relative object name, applying the
// same platform-independent key validation the filesystem store uses. The check
// is not about traversal here — S3 has no parent directory — but about keeping
// ONE definition of what a key is, so a key that is legal in one tier cannot be
// illegal in the other and strand content that has already been demoted.
func (s *S3Store) objectPath(key string) (string, error) {
	if !validKey(key) {
		return "", fmt.Errorf("invalid blob key %q", key)
	}
	if s.prefix == "" {
		return key, nil
	}
	return s.prefix + "/" + key, nil
}

func (s *S3Store) Put(ctx context.Context, r io.Reader) (*PutResult, error) {
	// The same random 16 bytes and two-level fan-out as FSStore, for the same
	// reason its comment gives: the content hash is not known until the stream
	// has been read, and buffering an arbitrary upload to learn it first is not
	// acceptable. The fan-out has no directory to keep small here, but keeping
	// both tiers on one key layout is what lets a demotion be a copy rather than
	// a rename of identity.
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate blob key: %w", err)
	}
	name := hex.EncodeToString(raw)
	key := fmt.Sprintf("%s/%s/%s", name[0:2], name[2:4], name)

	// The hash must be computed during the copy — the Store contract says the
	// data is never read twice — but the body also has to be signed or spooled.
	// It is spooled to a temp file: an S3 PUT is not restartable from a Reader
	// that has already been consumed, so a retry needs the bytes back, and
	// UNSIGNED-PAYLOAD (see sign) is what avoids a second full pass to hash it
	// for the signature.
	spool, size, sum, err := spoolAndHash(ctx, r)
	if err != nil {
		return nil, err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()

	if err := s.putObject(ctx, key, spool, size); err != nil {
		return nil, err
	}
	return &PutResult{Key: key, Size: size, SHA256: sum}, nil
}

// PutKeyed writes r at a caller-chosen key, skipping the write if the object is
// already there.
//
// Idempotent for exactly the reason the interface documents: the key IS the
// content hash, so an existing object holds byte for byte what would be
// written. The HEAD costs one round trip and saves an upload, which on a cold
// tier is also the thing the operator pays for by the gigabyte.
func (s *S3Store) PutKeyed(ctx context.Context, key string, r io.Reader) (bool, error) {
	if _, err := s.Stat(ctx, key); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}

	spool, size, _, err := spoolAndHash(ctx, r)
	if err != nil {
		return false, err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()

	if err := s.putObject(ctx, key, spool, size); err != nil {
		return false, err
	}
	return false, nil
}

// putObject sends one PUT. The body is a *os.File positioned at zero, so the
// request has a real Content-Length — object stores are entitled to refuse a
// chunked upload, and several of the self-hosted ones do.
func (s *S3Store) putObject(ctx context.Context, key string, body *os.File, size int64) error {
	obj, err := s.objectPath(key)
	if err != nil {
		return err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	req, err := s.newRequest(ctx, http.MethodPut, obj, nil, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	// GetBody lets the transport replay the body after a redirect or a retried
	// connection. Without it a re-dialled request sends an empty object, which
	// is the worst possible failure here: a "successful" PUT of nothing.
	req.GetBody = func() (io.ReadCloser, error) {
		f, err := os.Open(body.Name())
		return f, err
	}

	resp, err := s.do(req)
	if err != nil {
		return fmt.Errorf("cold tier put %s: %w", key, err)
	}
	defer drain(resp)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cold tier put %s: %w", key, s3Error(resp))
	}
	return nil
}

func (s *S3Store) Stat(ctx context.Context, key string) (int64, error) {
	obj, err := s.objectPath(key)
	if err != nil {
		return 0, err
	}
	req, err := s.newRequest(ctx, http.MethodHead, obj, nil, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.do(req)
	if err != nil {
		return 0, fmt.Errorf("cold tier stat %s: %w", key, err)
	}
	defer drain(resp)

	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("cold tier stat %s: %w", key, s3Error(resp))
	}
	// A HEAD carries no body, so the length has to come from the header. Some
	// gateways omit it on HEAD; treating that as zero would report a live object
	// as empty, so it is an error instead.
	n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cold tier stat %s: no usable Content-Length", key)
	}
	return n, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	obj, err := s.objectPath(key)
	if err != nil {
		return err
	}
	req, err := s.newRequest(ctx, http.MethodDelete, obj, nil, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return fmt.Errorf("cold tier delete %s: %w", key, err)
	}
	defer drain(resp)

	// S3 answers 204 whether or not the object was there, and a gateway that
	// answers 404 means the same thing. Already gone is the desired end state:
	// deleting twice must not be an error, or a GC retry becomes a permanent
	// failure — the same rule FSStore.Delete follows.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode/100 == 2 {
		return nil
	}
	return fmt.Errorf("cold tier delete %s: %w", key, s3Error(resp))
}

// Open returns a seekable reader over a cold object.
//
// It is an io.ReadSeekCloser because the Store contract says so, and the reason
// the contract says so applies here more than anywhere: Range requests must
// seek rather than discard a prefix, and discarding a prefix over the network
// means paying egress for bytes nobody asked for. Seeking is implemented by
// abandoning the current response and issuing a new ranged GET at the new
// offset, which is what a `Range: bytes=N-` header is for.
//
// The size is fetched up front with a HEAD, because io.SeekEnd cannot be
// answered without it and http.ServeContent seeks to the end to learn a length.
func (s *S3Store) Open(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	size, err := s.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	return &s3Reader{ctx: ctx, store: s, key: key, size: size}, nil
}

// Walk visits every object under the prefix, paging through ListObjectsV2.
//
// fsck uses it to find objects in the bucket that no database row references.
// It deliberately does NOT feed the repair path: see the note on Fsck about why
// an unreferenced cold object is reported and never deleted.
func (s *S3Store) Walk(ctx context.Context, fn func(key string, size int64) error) error {
	var token string
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("max-keys", "1000")
		if s.prefix != "" {
			q.Set("prefix", s.prefix+"/")
		}
		if token != "" {
			q.Set("continuation-token", token)
		}

		req, err := s.newRequest(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return err
		}
		resp, err := s.do(req)
		if err != nil {
			return fmt.Errorf("cold tier list: %w", err)
		}
		if resp.StatusCode/100 != 2 {
			err := s3Error(resp)
			drain(resp)
			return fmt.Errorf("cold tier list: %w", err)
		}

		var out listBucketResult
		err = xml.NewDecoder(resp.Body).Decode(&out)
		drain(resp)
		if err != nil {
			return fmt.Errorf("cold tier list: %w", err)
		}

		for _, c := range out.Contents {
			key := c.Key
			if s.prefix != "" {
				trimmed, ok := strings.CutPrefix(key, s.prefix+"/")
				if !ok {
					// Something else's content sharing the bucket. Not ours to
					// report on, and certainly not ours to act on.
					continue
				}
				key = trimmed
			}
			if err := fn(key, c.Size); err != nil {
				return err
			}
		}
		if !out.IsTruncated || out.NextContinuationToken == "" {
			return nil
		}
		token = out.NextContinuationToken
	}
}

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
}

// --- staging (resumable uploads) --------------------------------------------

// The Stager half is implemented so that S3Store satisfies the same contract
// FSStore does and can be tested against it, but it is NOT how this system
// takes uploads and should not become that.
//
// Uploads land HOT. That is the whole shape of a tiered store: content arrives
// on local disk at full speed, and only age and disuse move it. So the honest
// implementation here is the simple one — a staging object rewritten whole on
// each append — rather than the multipart-upload dance the Stager doc comment
// imagines. Multipart cannot express this interface anyway: AppendPartial
// truncates to an arbitrary offset and appends an arbitrary number of bytes,
// while multipart parts are immutable and all but the last must be at least
// 5 MiB. Choosing the shape that matches the interface, and paying a rewrite
// per append, is the tradeoff — and the reason the cold tier is never offered
// as the upload target.

const s3StagingPrefix = stagingDir + "/"

func (s *S3Store) CreatePartial() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate staging key: %w", err)
	}
	key := s3StagingPrefix + hex.EncodeToString(raw)

	// An explicit empty object, so StatPartial can distinguish "created, nothing
	// appended yet" from "never created" — which is what makes a resumed upload
	// able to ask where it got to.
	spool, err := os.CreateTemp("", "pc-s3-stage-*")
	if err != nil {
		return "", err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()
	if err := s.putObject(context.Background(), key, spool, 0); err != nil {
		return "", err
	}
	return key, nil
}

// AppendPartial truncates the staging object to offset, appends r, and updates
// hasher with the bytes written.
//
// Truncating first is the same rule FSStore follows and matters for the same
// reason: a crash between writing bytes and committing the new offset must
// leave the caller's recorded offset as the single authority, so a resumed
// upload cannot splice a duplicated span into the middle of a file.
func (s *S3Store) AppendPartial(ctx context.Context, key string, offset int64, hasher hash.Hash, r io.Reader) (int64, error) {
	if !strings.HasPrefix(key, s3StagingPrefix) {
		return 0, fmt.Errorf("%q is not a staging key", key)
	}
	existing, err := s.Stat(ctx, key)
	if err != nil {
		return 0, err
	}
	if offset > existing {
		return 0, fmt.Errorf("cannot resume at %d: the staging object holds %d bytes", offset, existing)
	}

	spool, err := os.CreateTemp("", "pc-s3-stage-*")
	if err != nil {
		return 0, err
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()

	if offset > 0 {
		rc, err := s.Open(ctx, key)
		if err != nil {
			return 0, err
		}
		_, err = io.Copy(spool, io.LimitReader(rc, offset))
		rc.Close()
		if err != nil {
			return 0, err
		}
	}

	written, copyErr := io.Copy(io.MultiWriter(spool, hasher), &ctxReader{ctx: ctx, r: r})
	// Upload whatever arrived even on a failed copy, for the same reason FSStore
	// fsyncs on a failed copy: the caller commits the offset it was told, and a
	// recorded offset whose bytes never landed is exactly the corruption
	// resumable uploads exist to avoid.
	if err := s.putObject(ctx, key, spool, offset+written); err != nil {
		return 0, err
	}
	return written, copyErr
}

// CommitPartial promotes a staging object to a real blob key.
//
// A server-side copy, not a download and re-upload: the bytes are already in the
// bucket, and pulling a multi-gigabyte object through this process to put it
// back would pay for the transfer twice and bound the file size by this
// machine's patience rather than the store's.
func (s *S3Store) CommitPartial(key string) (string, error) {
	if !strings.HasPrefix(key, s3StagingPrefix) {
		return "", fmt.Errorf("%q is not a staging key", key)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate blob key: %w", err)
	}
	name := hex.EncodeToString(raw)
	blobKey := fmt.Sprintf("%s/%s/%s", name[0:2], name[2:4], name)

	ctx := context.Background()
	src, err := s.objectPath(key)
	if err != nil {
		return "", err
	}
	dst, err := s.objectPath(blobKey)
	if err != nil {
		return "", err
	}

	req, err := s.newRequest(ctx, http.MethodPut, dst, nil, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-amz-copy-source", "/"+s.bucket+"/"+src)
	req.ContentLength = 0

	resp, err := s.do(req)
	if err != nil {
		return "", fmt.Errorf("commit staged upload: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("commit staged upload: %w", s3Error(resp))
	}

	// Only now is the staging object redundant. Removing it first would turn a
	// failed copy into a lost upload.
	if err := s.Delete(ctx, key); err != nil {
		return "", err
	}
	return blobKey, nil
}

func (s *S3Store) StatPartial(key string) (int64, error) { return s.Stat(context.Background(), key) }
func (s *S3Store) RemovePartial(key string) error        { return s.Delete(context.Background(), key) }

// WalkStaging visits every partial-upload object, so the GC can find staging
// objects whose session row has vanished.
func (s *S3Store) WalkStaging(fn func(key string, size int64) error) error {
	return s.Walk(context.Background(), func(key string, size int64) error {
		if !strings.HasPrefix(key, s3StagingPrefix) {
			return nil
		}
		return fn(key, size)
	})
}

// --- reader -----------------------------------------------------------------

// s3Reader streams one object, re-issuing a ranged GET whenever the caller
// seeks. The response body is opened lazily, so a ServeContent probe that seeks
// to the end and straight back costs headers rather than a transfer.
type s3Reader struct {
	ctx   context.Context
	store *S3Store
	key   string
	size  int64

	pos  int64
	body io.ReadCloser
	// bodyAt is the offset the open body starts at, so a sequential read can
	// keep using it instead of reconnecting for every Read call.
	bodyAt int64
	closed bool
}

func (r *s3Reader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errors.New("read after close")
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil || r.bodyAt != r.pos {
		if err := r.reopen(); err != nil {
			return 0, err
		}
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	r.bodyAt += int64(n)
	if errors.Is(err, io.EOF) && r.pos < r.size {
		// The connection ended early. Reporting EOF here would silently serve a
		// truncated file, which is the one failure mode a cold tier must never
		// have, so it is an error the caller can see.
		return n, fmt.Errorf("cold tier read %s: stream ended at %d of %d bytes", r.key, r.pos, r.size)
	}
	return n, err
}

func (r *s3Reader) reopen() error {
	r.closeBody()

	obj, err := r.store.objectPath(r.key)
	if err != nil {
		return err
	}
	req, err := r.store.newRequest(r.ctx, http.MethodGet, obj, nil, nil)
	if err != nil {
		return err
	}
	if r.pos > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.pos))
	}
	resp, err := r.store.do(req)
	if err != nil {
		return fmt.Errorf("cold tier get %s: %w", r.key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		e := s3Error(resp)
		drain(resp)
		return fmt.Errorf("cold tier get %s: %w", r.key, e)
	}
	// A store that ignores Range answers 200 with the whole object. Rather than
	// serve the wrong bytes from the wrong offset, discard the prefix — correct,
	// and slow enough that the wasted egress shows up in a bill rather than in
	// corrupted downloads.
	if r.pos > 0 && resp.StatusCode == http.StatusOK {
		if _, err := io.CopyN(io.Discard, resp.Body, r.pos); err != nil {
			drain(resp)
			return fmt.Errorf("cold tier get %s: %w", r.key, err)
		}
	}
	r.body, r.bodyAt = resp.Body, r.pos
	return nil
}

func (r *s3Reader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, errors.New("seek after close")
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	// Seeking past the end is legal and the next Read returns EOF, matching
	// os.File. ServeContent relies on that when probing for a length.
	r.pos = abs
	return abs, nil
}

func (r *s3Reader) closeBody() {
	if r.body != nil {
		// Closed without draining: the remainder of a ranged GET that is being
		// abandoned is egress nobody wants to pay for.
		r.body.Close()
		r.body = nil
	}
}

func (r *s3Reader) Close() error {
	r.closed = true
	r.closeBody()
	return nil
}

// --- request plumbing -------------------------------------------------------

func (s *S3Store) newRequest(ctx context.Context, method, obj string, query url.Values, body io.Reader) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The object name is escaped per path segment. url.URL does that itself when
	// given an unescaped Path, and RawPath keeps the escaped form the signature
	// was computed over — the two must agree or every request is a 403.
	raw := "/" + s.bucket
	if obj != "" {
		raw += "/" + obj
	}
	u, err := url.Parse(s.endpoint + escapePath(raw))
	if err != nil {
		return nil, err
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	return req, nil
}

func (s *S3Store) do(req *http.Request) (*http.Response, error) {
	if err := s.sign(req); err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

// unsignedPayload tells the service not to include a body digest in the
// signature.
//
// The alternative is hashing the whole body before sending it, which for a
// multi-gigabyte demotion means reading every byte twice — once to hash, once
// to send — for a property TLS already provides on the wire and the read-back
// verification in the tiering job provides end to end. Over plain HTTP to a
// MinIO on the same LAN it also holds, because the tiering job re-reads what it
// wrote and compares digests before it deletes anything local.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// sign adds the AWS Signature Version 4 headers.
//
// The algorithm is: build a canonical request, hash it, wrap that in a string
// to sign scoped to date/region/service, derive a key by a four-step HMAC chain
// from the secret, and sign. Every one of those steps is a place a subtle
// mistake produces exactly one symptom — 403 SignatureDoesNotMatch — which is
// why s3_test.go's fake service recomputes the signature rather than trusting
// the header to be present.
func (s *S3Store) sign(req *http.Request) error {
	t := s.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", unsignedPayload)
	if req.Host != "" {
		req.Header.Set("Host", req.Host)
	} else {
		req.Header.Set("Host", req.URL.Host)
	}

	// Sign host, both x-amz headers, and any other x-amz-* the caller added
	// (x-amz-copy-source, in CommitPartial's case). Signing the copy source
	// matters: unsigned, it is a header an intermediary could rewrite to copy
	// somebody else's object into this bucket.
	var signed []string
	canonHeaders := map[string]string{}
	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if lower != "host" && !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		canonHeaders[lower] = strings.TrimSpace(strings.Join(vals, ","))
		signed = append(signed, lower)
	}
	if _, ok := canonHeaders["host"]; !ok {
		canonHeaders["host"] = req.URL.Host
		signed = append(signed, "host")
	}
	sort.Strings(signed)

	var hb strings.Builder
	for _, h := range signed {
		hb.WriteString(h)
		hb.WriteString(":")
		hb.WriteString(canonHeaders[h])
		hb.WriteString("\n")
	}
	signedHeaders := strings.Join(signed, ";")

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery(req.URL.Query()),
		hb.String(),
		signedHeaders,
		unsignedPayload,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.region, "s3", "aws4_request"}, "/")
	crHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crHash[:]),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.secret), dateStamp)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.keyID, scope, signedHeaders, signature))
	return nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQuery renders the query string the way SigV4 requires: sorted by
// key, then value, with RFC 3986 escaping. url.Values.Encode is nearly right
// but does not sort duplicate values, so it is spelled out.
func canonicalQuery(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), v[k]...)
		sort.Strings(vals)
		for _, val := range vals {
			parts = append(parts, escapeQuery(k)+"="+escapeQuery(val))
		}
	}
	return strings.Join(parts, "&")
}

// escapeQuery is RFC 3986 unreserved-only escaping. url.QueryEscape encodes a
// space as "+", which SigV4 rejects, and leaves "+" itself alone.
func escapeQuery(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// escapePath escapes each path segment, keeping the separators. Store keys are
// hex and slashes today, so this is almost always a no-op — but "almost always"
// is not a property to sign a request on.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = escapeQuery(seg)
	}
	return strings.Join(segs, "/")
}

// spoolAndHash copies r to a temp file, returning the file, its length and the
// SHA-256 of its contents.
//
// Spooling rather than streaming straight to the socket is deliberate: an
// object PUT needs a Content-Length, and a Reader of unknown length can only
// supply one by being consumed first. The temp file is also what makes GetBody
// possible, so a redirected or retried request cannot silently PUT an empty
// object.
func spoolAndHash(ctx context.Context, r io.Reader) (*os.File, int64, []byte, error) {
	f, err := os.CreateTemp("", "pc-s3-put-*")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("spool for cold tier: %w", err)
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), &ctxReader{ctx: ctx, r: r})
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, nil, fmt.Errorf("spool for cold tier: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, nil, err
	}
	return f, n, hasher.Sum(nil), nil
}

// s3Error turns an error response into something an operator can act on. The
// body is XML with a Code and a Message; the code is the part worth surfacing,
// because "NoSuchBucket" and "SignatureDoesNotMatch" send you to entirely
// different settings.
func s3Error(resp *http.Response) error {
	var payload struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err := xml.Unmarshal(bytes.TrimSpace(body), &payload); err == nil && payload.Code != "" {
		return fmt.Errorf("%s: %s (HTTP %d)", payload.Code, payload.Message, resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d from the cold tier", resp.StatusCode)
}

// drain reads and closes a response body so the connection returns to the pool
// instead of being torn down after every request.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}
