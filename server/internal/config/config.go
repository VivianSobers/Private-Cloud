// Package config loads runtime configuration from the environment.
//
// Everything is read once at startup and validated eagerly: a misconfigured
// server should fail immediately and loudly, not three hours later on the first
// request that happens to touch the broken setting.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Env is "dev" or "prod". It only affects defaults and log formatting;
	// never use it to switch security behaviour, or dev becomes the untested
	// configuration that eventually ships.
	Env string

	HTTPAddr        string
	ShutdownTimeout time.Duration

	DatabaseURL      string
	DBMaxConns       int32
	DBMinConns       int32
	DBConnectTimeout time.Duration

	LogLevel  string
	LogFormat string // "json" or "text"

	// --- WebAuthn ---
	//
	// RPID is the "relying party ID": the domain passkeys are bound to. It must
	// be the registrable domain the browser sees, WITHOUT scheme or port
	// ("cloud.tail1234.ts.net"). Getting this wrong is the single most common
	// WebAuthn misconfiguration, and it fails at credential-creation time with
	// an opaque browser error rather than anything useful server-side.
	//
	// Changing RPID later INVALIDATES every existing passkey — they are bound
	// to the origin they were created against. Decide the hostname before
	// enrolling keys you care about.
	WebAuthnRPID    string
	WebAuthnRPName  string
	WebAuthnOrigins []string

	// --- sessions ---
	SessionTTL   time.Duration
	CookieName   string
	CookieSecure bool

	// --- storage ---
	//
	// BlobPath is where file content lives. It must be on the ZFS dataset that
	// Phase 0 set up (tank/data), because that is what sanoid snapshots and
	// restic backs up — a blob store outside it is silently unprotected.
	BlobPath string

	// TrashRetention bounds how long deleted files keep occupying disk.
	TrashRetention time.Duration
	// BlobGCInterval is how often unreferenced blobs are swept.
	BlobGCInterval time.Duration
	// UploadTTL bounds how long an abandoned resumable upload occupies disk.
	// Generous by default: a large file over a slow link legitimately spans
	// days of intermittent connectivity, and expiring it mid-transfer costs
	// the user everything they had already sent.
	UploadTTL time.Duration

	// MigrateOnStart runs pending migrations during startup. Convenient for a
	// single-node deployment; would be wrong with multiple replicas racing to
	// migrate, which is why it is a flag rather than unconditional.
	MigrateOnStart bool

	// BlobMigrateInterval is how often the background pass rewrites Phase 1
	// whole-file blobs into content-addressed chunks. Zero disables it — the
	// default, because a storage rewrite is exactly when a fresh backup should be
	// taken first, so draining the backlog is a deliberate operator act
	// (`cloudctl migrate-blobs`), not something that fires unbidden on deploy.
	BlobMigrateInterval time.Duration
	// BlobMigrateBatch bounds how many versions one pass rewrites, so the drain
	// stays incremental and a single tick cannot monopolise disk and CPU on a
	// small box for an unbounded time.
	BlobMigrateBatch int

	// KeepVersions is how many of a file's newest versions survive pruning
	// regardless of age; VersionRetention keeps anything younger than it whatever
	// its rank. A version is pruned only when it fails both. Forgiving by
	// default: with CAS, keeping history of an unchanged file is nearly free.
	KeepVersions     int
	VersionRetention time.Duration

	// ChangeRetention bounds the sync journal's tail. A client offline longer than
	// this re-syncs from scratch instead of resuming from a cursor.
	ChangeRetention time.Duration

	// Embed configures the semantic-search inference sidecar. Zero-valued (empty
	// URL) leaves semantic search off; lexical and OCR search are unaffected.
	Embed EmbedConfig

	// OIDC configures single sign-on. Empty Issuer leaves SSO off; the passkey
	// and recovery paths are unaffected either way.
	OIDC OIDCSettings

	// Push configures Web Push delivery. Empty leaves it off, and off is a
	// supported state: a client that cannot subscribe polls GET /changes, which
	// is what every client did before push existed.
	Push PushSettings

	// Media configures the optional external tooling the media job may use.
	// Empty leaves video thumbnails off; photo thumbnails, video metadata and
	// the timeline are unaffected either way.
	Media MediaSettings

	// Billing configures metering and the outbound billing webhook. Every field
	// is off or benign by default: the plan and metering endpoints work with no
	// configuration at all, and the webhook — the only part that talks to
	// anything outside this deployment — stays silent until an operator names an
	// endpoint for it.
	Billing BillingSettings

	// Cold configures the object-storage tier and the policy that demotes into
	// it. Off by default, and off means the blob store is exactly the local one
	// it has always been — no wrapper, no second location for fsck to reason
	// about, and no change to any read path.
	Cold ColdSettings
}

// MediaSettings names the external binaries the media job is allowed to run.
//
// One entry, and it is off by default. Video thumbnails are the only thing in
// this system that genuinely needs a video decoder — duration, dimensions,
// rotation and capture time are read in-process from the container header — so
// the decoder is a switch rather than a dependency, on the same terms OCR's
// tesseract is.
//
// One deliberate difference from OCR: tesseract is picked up from PATH, ffmpeg
// is not. ffmpeg is present on a great many machines as a transitive dependency
// of something unrelated, and video decoding is the largest hostile-input
// surface here. Turning it on because a library happened to install it would be
// an accident wearing the costume of an operator decision.
type MediaSettings struct {
	// FFmpegPath is the ffmpeg binary to invoke — an absolute path, or a bare
	// name resolved on PATH. Empty (the default) disables video thumbnails.
	FFmpegPath string
}

// VideoThumbnailsEnabled reports whether an operator asked for video
// thumbnails. It says nothing about whether the binary is actually there: that
// is resolved by media.NewThumbnailer and reported in the worker's startup log,
// because "configured but missing" and "not configured" behave identically and
// are very different mistakes.
func (m MediaSettings) VideoThumbnailsEnabled() bool { return m.FFmpegPath != "" }

// LoadMedia reads the media tooling settings on their own, for the worker —
// which deliberately does not load the API's full configuration, exactly as
// LoadEmbed exists so it need not.
func LoadMedia() (MediaSettings, error) {
	m := MediaSettings{FFmpegPath: env("PC_FFMPEG_PATH", "")}
	if err := m.validate(); err != nil {
		return MediaSettings{}, err
	}
	return m, nil
}

func (m MediaSettings) validate() error {
	if !m.VideoThumbnailsEnabled() {
		return nil
	}
	// A bare name (no separator) is a PATH lookup and is fine. A path WITH
	// separators must be absolute, for the reason PC_BLOB_PATH must be: a
	// relative path resolves against whatever the working directory happens to
	// be, which differs between `make run`, the container and systemd — and a
	// relative path to an executable is also how the wrong binary gets run.
	if strings.ContainsAny(m.FFmpegPath, `/\`) && !filepath.IsAbs(m.FFmpegPath) {
		return fmt.Errorf("PC_FFMPEG_PATH must be an absolute path or a bare command name (got %q)", m.FFmpegPath)
	}
	// Existence is deliberately NOT checked here. A missing binary degrades to
	// "no video thumbnails", which is the same state the default is in and
	// survivable; refusing to start the worker over it would take OCR, tagging
	// and embedding down with it.
	return nil
}

// BillingSettings configures Phase 9's metering and the outbound billing hook.
//
// The webhook is the only outbound capability in this file, and it is off by
// default for the reason a self-hosted server should be: a deployment with one
// account has nothing to tell anybody, and an HTTP call it never needed is a
// capability an attacker would rather it had. Metering is separately switchable
// because it costs a query per account per tick and a deployment that will never
// bill for anything may reasonably decline to pay it.
type BillingSettings struct {
	// MeterInterval is how often the worker writes a usage snapshot per owner.
	// Zero disables metering entirely; the plan endpoints keep working, they
	// simply have nothing historical to report.
	//
	// The default is hourly rather than daily because the record's job is to make
	// a CLOSED period answerable, and the peak within a period is only as
	// truthful as the sampling that observed it.
	MeterInterval time.Duration

	// Period is the calendar grain a billing period is cut on: "month" (the
	// default) or "day". Day exists so the period-closing path can be exercised
	// without waiting a month for it.
	Period string

	// WebhookURL is where billing events are POSTed. Empty — the default —
	// disables delivery, and nothing else changes.
	WebhookURL string
	// WebhookSecret keys the HMAC every delivery is signed with. Required when a
	// URL is set: an unsigned webhook is an endpoint anybody who can reach it can
	// forge a plan change into, and "we will add signing later" is how that ships.
	WebhookSecret string
	// WebhookTimeout bounds one delivery attempt.
	WebhookTimeout time.Duration
	// WebhookAttempts is how many times one event is tried before it is dropped
	// and logged. A 4xx stops the retries immediately regardless — repeating a
	// request the receiver has already called wrong cannot make it right.
	WebhookAttempts int
}

// WebhookEnabled reports whether billing events will be delivered anywhere.
func (b BillingSettings) WebhookEnabled() bool { return b.WebhookURL != "" }

// MeteringEnabled reports whether the worker should take usage snapshots.
func (b BillingSettings) MeteringEnabled() bool { return b.MeterInterval > 0 }

// LoadBilling reads the billing settings on their own, for the worker — which
// deliberately does not load the API's full configuration, exactly as LoadEmbed
// and LoadMedia exist so it need not.
func LoadBilling() (BillingSettings, error) {
	b := BillingSettings{
		MeterInterval:   envDuration("PC_BILLING_METER_INTERVAL", time.Hour),
		Period:          env("PC_BILLING_PERIOD", "month"),
		WebhookURL:      env("PC_BILLING_WEBHOOK_URL", ""),
		WebhookSecret:   env("PC_BILLING_WEBHOOK_SECRET", ""),
		WebhookTimeout:  envDuration("PC_BILLING_WEBHOOK_TIMEOUT", 10*time.Second),
		WebhookAttempts: envInt("PC_BILLING_WEBHOOK_ATTEMPTS", 4),
	}
	if err := b.validate(); err != nil {
		return BillingSettings{}, err
	}
	return b, nil
}

func (b BillingSettings) validate() error {
	// The grain is validated even when metering is off, because turning metering
	// on later must not be the moment a typo in an unrelated variable is
	// discovered.
	switch b.Period {
	case "", "month", "day":
	default:
		return fmt.Errorf("PC_BILLING_PERIOD must be month or day (got %q)", b.Period)
	}
	if !b.WebhookEnabled() {
		// A secret with no URL is harmless but is almost certainly half of an
		// intended configuration, so say so rather than starting silently.
		if b.WebhookSecret != "" {
			return fmt.Errorf("PC_BILLING_WEBHOOK_SECRET is set but PC_BILLING_WEBHOOK_URL is not: nothing would be delivered")
		}
		return nil
	}
	if !strings.HasPrefix(b.WebhookURL, "http://") && !strings.HasPrefix(b.WebhookURL, "https://") {
		return fmt.Errorf("PC_BILLING_WEBHOOK_URL must be an http:// or https:// address (got %q)", b.WebhookURL)
	}
	// The refusal that matters. A webhook nobody can authenticate is one anybody
	// who can reach the receiver can forge a plan change into, and the receiver
	// has no way to tell the difference.
	if len(b.WebhookSecret) < 16 {
		return fmt.Errorf("PC_BILLING_WEBHOOK_SECRET must be at least 16 characters when PC_BILLING_WEBHOOK_URL is set: an unsigned or weakly-signed hook is one anybody who can reach the receiver can forge")
	}
	if b.WebhookTimeout <= 0 {
		return fmt.Errorf("PC_BILLING_WEBHOOK_TIMEOUT must be positive")
	}
	if b.WebhookAttempts < 1 {
		return fmt.Errorf("PC_BILLING_WEBHOOK_ATTEMPTS must be at least 1")
	}
	return nil
}

// PushSettings is the VAPID identity this server signs notifications with.
//
// The private key is configuration rather than something generated at startup
// because the PUBLIC half is baked into every subscription a browser has already
// created — PushManager.subscribe binds a subscription to the applicationServer
// Key it was handed, and a push signed by a different key is refused. A key
// regenerated on restart would therefore silently invalidate every subscription
// on every deploy. `cloudctl push keygen` mints one to paste in.
type PushSettings struct {
	// PrivateKey is the base64url P-256 scalar. The public half is derived from
	// it, so a mismatched pair cannot be configured.
	PrivateKey string
	// Subject is a mailto: or https: URL identifying the operator, which push
	// services require as an abuse contact.
	Subject string
}

// Enabled reports whether push delivery is configured.
func (p PushSettings) Enabled() bool { return p.PrivateKey != "" }

// EmbedConfig points at the embedding inference sidecar.
//
// Shared by the API (which embeds the QUERY) and the worker (which embeds
// DOCUMENTS), so both read one definition and one validator rather than each
// binary parsing the same three variables its own way.
type EmbedConfig struct {
	// URL is the sidecar's base address. Empty disables embedding here.
	URL string
	// Model is the identity vectors are stored under. Vectors are only comparable
	// within one model, so this is written with every row and filtered on at query
	// time; it must not change without a re-index.
	Model string
	// Dim is the vector width the sidecar produces. Vectors of another width are
	// rejected on arrival and excluded at query time.
	Dim int
	// EnableSemantic makes the worker chain extraction to embedding even when it
	// has no sidecar of its own — the always-on box feeding a separate GPU worker.
	// Meaningless to the API, which never enqueues.
	EnableSemantic bool

	// GenerateURL points at the generation sidecar that composes written answers
	// for POST /chat. Separate from URL because the two are different services
	// with very different resource profiles — an embedder is a single forward
	// pass and can share a modest box, a generator holds a much larger model —
	// and a deployment may reasonably run one and not the other.
	//
	// Empty leaves /chat in retrieval-only mode: it still returns the passages,
	// which is the trustworthy half of RAG and useful on its own.
	GenerateURL string
	// GenerateModel is advisory, reported alongside an answer so a reader knows
	// what produced it. The sidecar is the authority on what it actually loaded.
	GenerateModel string

	// DetectURL points at the face-detection sidecar. A third service again,
	// because a deployment may reasonably want thumbnails and search without
	// wanting a face model — and folding detection into the media job would tie
	// thumbnailing, which everyone wants, to a sidecar most will not run.
	DetectURL string
	// DetectModel is the identity face vectors are stored under, and like the
	// embedding model it must not change without re-detecting: vectors from two
	// models compared as if in one space cluster strangers together.
	DetectModel string
	// DetectDim is the face-vector width the sidecar produces.
	DetectDim int

	// ImageURL points at the image-embedding sidecar — the fourth service, and
	// the one that gives a photograph with no text any neighbours at all.
	// Separate from URL for the reason DetectURL is: a vision encoder is a
	// different model with a different resource profile, and a deployment may
	// reasonably want semantic document search without one.
	//
	// Empty leaves /nodes/{id}/similar ranking in the document space exactly as
	// it did before this space existed.
	ImageURL string
	// ImageModel is the identity image vectors are stored under. Like the
	// embedding model it must not change without re-embedding: vectors from two
	// models compared as if in one space rank noise.
	ImageModel string
	// ImageDim is the image-vector width the sidecar produces.
	ImageDim int
}

// DetectionEnabled reports whether face detection is configured.
func (e EmbedConfig) DetectionEnabled() bool { return e.DetectURL != "" }

// ImageEmbeddingEnabled reports whether the image-embedding space is configured.
func (e EmbedConfig) ImageEmbeddingEnabled() bool { return e.ImageURL != "" }

// GenerationEnabled reports whether written answers are configured.
func (e EmbedConfig) GenerationEnabled() bool { return e.GenerateURL != "" }

// Enabled reports whether a sidecar is configured here.
func (e EmbedConfig) Enabled() bool { return e.URL != "" }

// OIDCSettings configures the single sign-on provider.
type OIDCSettings struct {
	// Issuer is the provider's discovery URL. Empty disables SSO.
	Issuer         string
	ClientID       string
	ClientSecret   string
	RedirectURL    string
	AllowedDomains []string
}

// Enabled reports whether a provider is configured.
func (o OIDCSettings) Enabled() bool { return o.Issuer != "" }

// LoadEmbed reads and validates the sidecar settings on their own, for the
// worker — which deliberately does not load the API's full configuration,
// because WebAuthn origins and blob paths are none of its business.
func LoadEmbed() (EmbedConfig, error) {
	e := EmbedConfig{
		URL:            env("PC_EMBED_URL", ""),
		Model:          env("PC_EMBED_MODEL", "bge-small-en-v1.5"),
		Dim:            envInt("PC_EMBED_DIM", 384),
		EnableSemantic: envBool("PC_ENABLE_SEMANTIC", false),
		GenerateURL:    env("PC_GENERATE_URL", ""),
		GenerateModel:  env("PC_GENERATE_MODEL", ""),
		DetectURL:      env("PC_DETECT_URL", ""),
		DetectModel:    env("PC_DETECT_MODEL", "facenet"),
		DetectDim:      envInt("PC_DETECT_DIM", 512),
		ImageURL:       env("PC_IMAGE_EMBED_URL", ""),
		ImageModel:     env("PC_IMAGE_EMBED_MODEL", "clip-vit-base-patch32"),
		ImageDim:       envInt("PC_IMAGE_EMBED_DIM", 512),
	}
	if err := e.validate(); err != nil {
		return EmbedConfig{}, err
	}
	return e, nil
}

func (e EmbedConfig) validate() error {
	// Checked before the embedder gate: a generator configured without an
	// embedder can never retrieve anything to ground an answer in, and would
	// answer every question from nothing at all — the one failure mode this
	// design refuses to ship.
	if e.GenerationEnabled() {
		if !strings.HasPrefix(e.GenerateURL, "http://") && !strings.HasPrefix(e.GenerateURL, "https://") {
			return fmt.Errorf("PC_GENERATE_URL must be an http:// or https:// address (got %q)", e.GenerateURL)
		}
		if strings.HasSuffix(e.GenerateURL, "/") {
			return fmt.Errorf("PC_GENERATE_URL must not end in a slash (got %q)", e.GenerateURL)
		}
		if !e.Enabled() {
			return fmt.Errorf("PC_GENERATE_URL is set but PC_EMBED_URL is not: " +
				"answers are grounded in retrieved documents, and without embeddings " +
				"there is nothing to retrieve")
		}
	}

	// Face detection is independent of the document embedder: it has its own
	// model, its own vector space and its own table, and a deployment may run one
	// without the other.
	if e.DetectionEnabled() {
		if !strings.HasPrefix(e.DetectURL, "http://") && !strings.HasPrefix(e.DetectURL, "https://") {
			return fmt.Errorf("PC_DETECT_URL must be an http:// or https:// address (got %q)", e.DetectURL)
		}
		if strings.HasSuffix(e.DetectURL, "/") {
			return fmt.Errorf("PC_DETECT_URL must not end in a slash (got %q)", e.DetectURL)
		}
		if e.DetectModel == "" {
			return fmt.Errorf("PC_DETECT_MODEL must not be empty: it is the identity every face vector is stored under")
		}
		if e.DetectDim <= 0 {
			return fmt.Errorf("PC_DETECT_DIM must be positive (got %d)", e.DetectDim)
		}
	}

	// Image embedding is independent of the document embedder too, and
	// deliberately NOT cross-validated against it the way generation is. A
	// generator with no embedder can only answer from nothing, which is a broken
	// feature; an image space with no document space is a coherent deployment —
	// a photo library where "find files like this one" works on pictures and
	// simply reports 404 not_indexed for a text file, which is what it would
	// report anyway.
	if e.ImageEmbeddingEnabled() {
		if !strings.HasPrefix(e.ImageURL, "http://") && !strings.HasPrefix(e.ImageURL, "https://") {
			return fmt.Errorf("PC_IMAGE_EMBED_URL must be an http:// or https:// address (got %q)", e.ImageURL)
		}
		if strings.HasSuffix(e.ImageURL, "/") {
			return fmt.Errorf("PC_IMAGE_EMBED_URL must not end in a slash (got %q)", e.ImageURL)
		}
		if e.ImageModel == "" {
			return fmt.Errorf("PC_IMAGE_EMBED_MODEL must not be empty: it is the identity every image vector is stored under")
		}
		if e.ImageDim <= 0 {
			return fmt.Errorf("PC_IMAGE_EMBED_DIM must be positive (got %d)", e.ImageDim)
		}
		// The one combination that is genuinely wrong. Both spaces are keyed by
		// (content hash, model), so a shared identity would put document vectors
		// and image vectors in tables that agree on the name of a space they do
		// not share — and `cloudctl embeddings status` would report one model at
		// two widths, which is the shape of a corrupted space rather than of two
		// healthy ones.
		if e.Enabled() && e.ImageModel == e.Model {
			return fmt.Errorf("PC_IMAGE_EMBED_MODEL and PC_EMBED_MODEL are both %q: "+
				"the two spaces are separate and must be named separately, or neither can be told apart in the tooling", e.Model)
		}
	}

	if !e.Enabled() {
		return nil
	}
	if !strings.HasPrefix(e.URL, "http://") && !strings.HasPrefix(e.URL, "https://") {
		return fmt.Errorf("PC_EMBED_URL must be an http:// or https:// address (got %q)", e.URL)
	}
	// A trailing slash would produce "…//embed", which some servers 404 on.
	if strings.HasSuffix(e.URL, "/") {
		return fmt.Errorf("PC_EMBED_URL must not end in a slash (got %q)", e.URL)
	}
	if e.Model == "" {
		return fmt.Errorf("PC_EMBED_MODEL must not be empty: it is the identity every vector is stored under")
	}
	// A non-positive dimension produces zero-width vectors that compare equal to
	// nothing and silently return no results, which is indistinguishable from an
	// empty corpus.
	if e.Dim <= 0 {
		return fmt.Errorf("PC_EMBED_DIM must be positive (got %d)", e.Dim)
	}
	return nil
}

func (o OIDCSettings) validate() error {
	if !o.Enabled() {
		return nil
	}
	if !strings.HasPrefix(o.Issuer, "https://") && !strings.HasPrefix(o.Issuer, "http://") {
		return fmt.Errorf("PC_OIDC_ISSUER must be an https:// URL (got %q)", o.Issuer)
	}
	// Discovery succeeds without these, and the failure only surfaces later as an
	// opaque redirect back to the login page — exactly the "three hours later"
	// failure this package exists to prevent.
	if o.ClientID == "" {
		return fmt.Errorf("PC_OIDC_CLIENT_ID is required when PC_OIDC_ISSUER is set")
	}
	if o.ClientSecret == "" {
		return fmt.Errorf("PC_OIDC_CLIENT_SECRET is required when PC_OIDC_ISSUER is set")
	}
	if !strings.HasPrefix(o.RedirectURL, "https://") && !strings.HasPrefix(o.RedirectURL, "http://") {
		return fmt.Errorf("PC_OIDC_REDIRECT_URL must be the full callback URL, with a scheme (got %q)", o.RedirectURL)
	}
	// The callback route is fixed; a redirect URL pointing anywhere else means the
	// provider will send the browser somewhere that cannot complete the flow.
	if !strings.HasSuffix(o.RedirectURL, oidcCallbackPath) {
		return fmt.Errorf("PC_OIDC_REDIRECT_URL must end in %s (got %q)", oidcCallbackPath, o.RedirectURL)
	}
	return nil
}

// oidcCallbackPath is the route handleOIDCCallback is registered on. Duplicated
// as a literal rather than imported, because config must not depend on httpapi.
const oidcCallbackPath = "/api/v1/auth/oidc/callback"

func Load() (*Config, error) {
	c := &Config{
		Env:             env("PC_ENV", "dev"),
		HTTPAddr:        env("PC_HTTP_ADDR", ":8080"),
		ShutdownTimeout: envDuration("PC_SHUTDOWN_TIMEOUT", 20*time.Second),

		DatabaseURL:      env("PC_DATABASE_URL", ""),
		DBMaxConns:       int32(envInt("PC_DB_MAX_CONNS", 10)),
		DBMinConns:       int32(envInt("PC_DB_MIN_CONNS", 2)),
		DBConnectTimeout: envDuration("PC_DB_CONNECT_TIMEOUT", 10*time.Second),

		LogLevel:  env("PC_LOG_LEVEL", "info"),
		LogFormat: env("PC_LOG_FORMAT", "json"),

		WebAuthnRPID:    env("PC_WEBAUTHN_RPID", "localhost"),
		WebAuthnRPName:  env("PC_WEBAUTHN_RP_NAME", "Private Cloud"),
		WebAuthnOrigins: envList("PC_WEBAUTHN_ORIGINS", []string{"http://localhost:8080"}),

		SessionTTL: envDuration("PC_SESSION_TTL", 30*24*time.Hour),
		CookieName: env("PC_COOKIE_NAME", "pc_session"),
		// Secure cookies by default; only a dev run over plain HTTP should
		// ever turn this off, and it must be an explicit act.
		CookieSecure: envBool("PC_COOKIE_SECURE", true),

		BlobPath:       env("PC_BLOB_PATH", "/data/blobs"),
		TrashRetention: envDuration("PC_TRASH_RETENTION", 30*24*time.Hour),
		BlobGCInterval: envDuration("PC_BLOB_GC_INTERVAL", 6*time.Hour),
		UploadTTL:      envDuration("PC_UPLOAD_TTL", 48*time.Hour),

		MigrateOnStart: envBool("PC_MIGRATE_ON_START", true),

		// Off by default: the operator drains Phase 1 blobs deliberately, after a
		// backup, rather than having a deploy start rewriting storage on its own.
		BlobMigrateInterval: envDuration("PC_BLOB_MIGRATE_INTERVAL", 0),
		BlobMigrateBatch:    envInt("PC_BLOB_MIGRATE_BATCH", 100),

		KeepVersions:     envInt("PC_KEEP_VERSIONS", 25),
		VersionRetention: envDuration("PC_VERSION_RETENTION", 90*24*time.Hour),
		ChangeRetention:  envDuration("PC_CHANGE_RETENTION", 30*24*time.Hour),

		OIDC: OIDCSettings{
			Issuer:         env("PC_OIDC_ISSUER", ""),
			ClientID:       env("PC_OIDC_CLIENT_ID", ""),
			ClientSecret:   env("PC_OIDC_CLIENT_SECRET", ""),
			RedirectURL:    env("PC_OIDC_REDIRECT_URL", ""),
			AllowedDomains: envList("PC_OIDC_ALLOWED_DOMAINS", nil),
		},

		Push: PushSettings{
			PrivateKey: env("PC_VAPID_PRIVATE_KEY", ""),
			Subject:    env("PC_VAPID_SUBJECT", ""),
		},
	}

	embed, err := LoadEmbed()
	if err != nil {
		return nil, err
	}
	c.Embed = embed

	media, err := LoadMedia()
	if err != nil {
		return nil, err
	}
	c.Media = media

	billing, err := LoadBilling()
	if err != nil {
		return nil, err
	}
	c.Billing = billing

	cold, err := LoadCold()
	if err != nil {
		return nil, err
	}
	c.Cold = cold

	// Unparseable values first: a validate() message about a setting the operator
	// never actually set is more confusing than the parse error that caused it.
	if err := TakeParseErrors(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("PC_DATABASE_URL is required")
	}
	// Catch the copy-paste mistake where the URL is a bare hostname.
	if !strings.HasPrefix(c.DatabaseURL, "postgres://") &&
		!strings.HasPrefix(c.DatabaseURL, "postgresql://") {
		return fmt.Errorf("PC_DATABASE_URL must be a postgres:// URL")
	}
	switch c.Env {
	case "dev", "prod":
	default:
		return fmt.Errorf("PC_ENV must be dev or prod, got %q", c.Env)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("PC_LOG_FORMAT must be json or text, got %q", c.LogFormat)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("PC_LOG_LEVEL must be debug/info/warn/error, got %q", c.LogLevel)
	}
	if c.DBMinConns > c.DBMaxConns {
		return fmt.Errorf("PC_DB_MIN_CONNS (%d) exceeds PC_DB_MAX_CONNS (%d)", c.DBMinConns, c.DBMaxConns)
	}

	// A scheme or port in RPID is the classic WebAuthn misconfiguration, and
	// the browser-side failure it produces is famously unhelpful. Reject it
	// here, where the message can actually say what is wrong.
	if strings.Contains(c.WebAuthnRPID, "://") || strings.Contains(c.WebAuthnRPID, ":") ||
		strings.Contains(c.WebAuthnRPID, "/") {
		return fmt.Errorf("PC_WEBAUTHN_RPID must be a bare domain with no scheme, port or path (got %q)", c.WebAuthnRPID)
	}
	if len(c.WebAuthnOrigins) == 0 {
		return fmt.Errorf("PC_WEBAUTHN_ORIGINS must list at least one origin")
	}
	// Origins are the mirror image: they MUST carry a scheme.
	for _, o := range c.WebAuthnOrigins {
		if !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
			return fmt.Errorf("PC_WEBAUTHN_ORIGINS entries must include a scheme (got %q)", o)
		}
	}

	// Refuse the combination that silently ships session cookies in the clear.
	if c.Env == "prod" && !c.CookieSecure {
		return fmt.Errorf("PC_COOKIE_SECURE=false is not allowed when PC_ENV=prod")
	}

	if c.BlobPath == "" {
		return fmt.Errorf("PC_BLOB_PATH is required")
	}
	// A relative path resolves against whatever the working directory happens
	// to be, which differs between `make run`, the container and systemd. For
	// the one directory holding every byte the user owns, that is not a risk
	// worth taking.
	if !filepath.IsAbs(c.BlobPath) {
		return fmt.Errorf("PC_BLOB_PATH must be an absolute path (got %q)", c.BlobPath)
	}
	// A retention of zero would purge the trash on its first sweep, which is
	// indistinguishable from having no trash at all.
	if c.TrashRetention <= 0 {
		return fmt.Errorf("PC_TRASH_RETENTION must be positive")
	}
	if c.BlobGCInterval <= 0 {
		return fmt.Errorf("PC_BLOB_GC_INTERVAL must be positive")
	}
	if c.UploadTTL <= 0 {
		return fmt.Errorf("PC_UPLOAD_TTL must be positive")
	}
	// The interval is allowed to be zero — that is how the background drain is
	// disabled — but an enabled loop with a non-positive batch would list zero
	// candidates every tick and never make progress, a silent no-op worse than
	// an error.
	if c.BlobMigrateInterval > 0 && c.BlobMigrateBatch <= 0 {
		return fmt.Errorf("PC_BLOB_MIGRATE_BATCH must be positive when PC_BLOB_MIGRATE_INTERVAL is set")
	}
	// A keep-count below one could prune a file's only non-head history on the
	// first sweep; retention must be positive so "younger than the window" is a
	// meaningful test rather than pruning everything immediately.
	if c.KeepVersions < 1 {
		return fmt.Errorf("PC_KEEP_VERSIONS must be at least 1")
	}
	if c.VersionRetention <= 0 {
		return fmt.Errorf("PC_VERSION_RETENTION must be positive")
	}
	if c.ChangeRetention <= 0 {
		return fmt.Errorf("PC_CHANGE_RETENTION must be positive")
	}
	if err := c.OIDC.validate(); err != nil {
		return err
	}
	return nil
}

// Redacted returns the config with the database password masked, for logging.
// Logging a config struct verbatim is how credentials end up in Loki.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"env":                   c.Env,
		"http_addr":             c.HTTPAddr,
		"database_url":          redactURL(c.DatabaseURL),
		"db_max_conns":          c.DBMaxConns,
		"log_level":             c.LogLevel,
		"log_format":            c.LogFormat,
		"blob_path":             c.BlobPath,
		"trash_retention":       c.TrashRetention.String(),
		"migrate_on_start":      c.MigrateOnStart,
		"blob_migrate_interval": c.BlobMigrateInterval.String(),
		"blob_migrate_batch":    c.BlobMigrateBatch,
		"keep_versions":         c.KeepVersions,
		"version_retention":     c.VersionRetention.String(),
		"change_retention":      c.ChangeRetention.String(),

		// Phase 4 features, so the startup log shows whether they are on and what
		// they are pointed at. The client secret is deliberately absent rather
		// than masked: a value that never reaches the log cannot leak from it.
		"semantic_enabled":     c.Embed.Enabled(),
		"embed_url":            c.Embed.URL,
		"embed_model":          c.Embed.Model,
		"embed_dim":            c.Embed.Dim,
		"oidc_enabled":         c.OIDC.Enabled(),
		"oidc_issuer":          c.OIDC.Issuer,
		"oidc_client_id":       c.OIDC.ClientID,
		"oidc_redirect_url":    c.OIDC.RedirectURL,
		"oidc_allowed_domains": c.OIDC.AllowedDomains,

		// Phase 5's one optional binary, for the same reason: a video with no
		// tile and a log that never mentions why is a bug report waiting to
		// happen.
		"video_thumbnails": c.Media.VideoThumbnailsEnabled(),
		"ffmpeg_path":      c.Media.FFmpegPath,

		// The cold tier's location, never its credentials. Where content went is
		// the first thing an operator needs from a startup log; the key that
		// opens it is the thing that must never appear in one.
		"cold_tier_enabled":  c.Cold.ColdTierEnabled(),
		"cold_tier_endpoint": c.Cold.Endpoint,
		"cold_tier_bucket":   c.Cold.Bucket,
	}
}

// redactURL masks the password in a postgres URL, preserving enough of the
// rest to be useful when debugging "why can't it connect".
func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	scheme := strings.Index(raw, "://")
	if scheme < 0 {
		return "***"
	}
	creds := raw[scheme+3 : at]
	colon := strings.Index(creds, ":")
	if colon < 0 {
		return raw // no password present
	}
	return raw[:scheme+3] + creds[:colon] + ":***" + raw[at:]
}

// The env helpers below are the single definition for the whole server: cmd/api
// and cmd/pcworker each had their own near-copies, which disagreed with these on
// the one thing that matters.
//
// An unparseable value is now REPORTED, not silently replaced by the default.
// This package promises that a misconfigured server "should fail immediately and
// loudly, not three hours later", and swallowing PC_DB_MAX_CONNS=ten broke that
// promise in the quietest possible way: the server starts, works, and is running
// settings the operator did not choose and has no way to see. The cmd/* copies
// at least logged a warning; nothing here did even that.
//
// Errors accumulate in parseErrs so one pass reports every bad variable, rather
// than making an operator fix them one restart at a time.
var parseErrs []error

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		parseErrs = append(parseErrs, fmt.Errorf("%s must be an integer, got %q", key, v))
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		parseErrs = append(parseErrs, fmt.Errorf("%s must be true or false, got %q", key, v))
		return def
	}
	return b
}

// envList parses a comma-separated list, trimming whitespace and dropping
// empties so a trailing comma doesn't become an empty origin.
func envList(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// EnvDuration reads a duration variable. Exported because cmd/pcworker needs it
// for its own knobs (PC_WORKER_IDLE, PC_WORKER_LEASE, PC_JOB_RETENTION) and had
// grown a private copy that silently ignored a bad value.
func EnvDuration(key string, def time.Duration) time.Duration {
	return envDuration(key, def)
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		parseErrs = append(parseErrs, fmt.Errorf("%s must be a duration such as 30s or 24h, got %q", key, v))
		return def
	}
	return d
}

// TakeParseErrors returns and clears the accumulated parse failures. Callers
// that read settings outside Load — the worker — check this themselves.
func TakeParseErrors() error {
	if len(parseErrs) == 0 {
		return nil
	}
	err := errors.Join(parseErrs...)
	parseErrs = nil
	return err
}

// --- cold tier (Phase 9 slice 3) ---------------------------------------------

// ColdSettings is the object-storage tier and the policy that demotes into it.
//
// Loaded on its own, like the embed, media and billing settings, because the
// worker is the process that runs the demotion sweep and it deliberately does
// not load the API's full configuration.
type ColdSettings struct {
	// Enabled is the master switch, separate from the S3 settings being present.
	// A configuration is a thing an operator edits over days: leaving the bucket
	// details in place while turning the tier off has to be possible, or the only
	// way to stop demoting is to delete the credentials.
	Enabled bool

	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// Prefix lets one bucket hold the cold tiers of more than one deployment
	// without their key spaces colliding.
	Prefix string

	// The policy. All three thresholds must be met before anything moves; see
	// jobs/tiering.Policy for why each one is there.
	MinAge  time.Duration
	MinIdle time.Duration
	MinSize int64
	Batch   int
}

// ColdTierEnabled reports whether this process should build a cold store at all.
func (c ColdSettings) ColdTierEnabled() bool { return c.Enabled }

// LoadCold reads the cold-tier settings.
//
// The defaults are the conservative ones from tiering.DefaultPolicy, and they
// are deliberately the same numbers rather than a second set that could drift:
// an operator turning this on for the first time should find that almost
// nothing qualifies and then loosen it on purpose. The opposite mistake drains
// the pool onto an uplink nobody has tested at that volume.
func LoadCold() (ColdSettings, error) {
	c := ColdSettings{
		Enabled:   envBool("PC_COLD_TIER_ENABLED", false),
		Endpoint:  env("PC_COLD_S3_ENDPOINT", ""),
		Bucket:    env("PC_COLD_S3_BUCKET", ""),
		Region:    env("PC_COLD_S3_REGION", "us-east-1"),
		AccessKey: env("PC_COLD_S3_ACCESS_KEY", ""),
		SecretKey: env("PC_COLD_S3_SECRET_KEY", ""),
		Prefix:    env("PC_COLD_S3_PREFIX", ""),

		MinAge:  envDuration("PC_COLD_TIER_MIN_AGE", 90*24*time.Hour),
		MinIdle: envDuration("PC_COLD_TIER_MIN_IDLE", 90*24*time.Hour),
		MinSize: int64(envInt("PC_COLD_TIER_MIN_SIZE", 1<<20)),
		Batch:   envInt("PC_COLD_TIER_BATCH", 100),
	}
	if err := c.validate(); err != nil {
		return ColdSettings{}, err
	}
	return c, nil
}

func (c ColdSettings) validate() error {
	if !c.Enabled {
		// Half a configuration is worth naming even when it is inert, for the
		// same reason a webhook secret with no URL is: it is almost always an
		// intention that did not finish, and discovering it at the moment
		// somebody turns the tier on is discovering it at the worst moment.
		if c.Bucket != "" && c.Endpoint == "" {
			return fmt.Errorf("PC_COLD_S3_BUCKET is set but PC_COLD_S3_ENDPOINT is not, and PC_COLD_TIER_ENABLED is false: nothing would be tiered")
		}
		return nil
	}
	// Refusals rather than defaults, on every field that decides WHERE content
	// goes. A cold tier pointed at the wrong bucket is not a condition to
	// discover later — by the time it is visible, the bytes have already moved.
	if c.Endpoint == "" {
		return fmt.Errorf("PC_COLD_TIER_ENABLED is set but PC_COLD_S3_ENDPOINT is empty")
	}
	if !strings.HasPrefix(c.Endpoint, "http://") && !strings.HasPrefix(c.Endpoint, "https://") {
		return fmt.Errorf("PC_COLD_S3_ENDPOINT must be an http:// or https:// address (got %q)", c.Endpoint)
	}
	if c.Bucket == "" {
		return fmt.Errorf("PC_COLD_TIER_ENABLED is set but PC_COLD_S3_BUCKET is empty")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return fmt.Errorf("PC_COLD_TIER_ENABLED is set but the PC_COLD_S3 credentials are incomplete")
	}
	if c.MinAge <= 0 {
		return fmt.Errorf("PC_COLD_TIER_MIN_AGE must be positive")
	}
	if c.MinIdle <= 0 {
		return fmt.Errorf("PC_COLD_TIER_MIN_IDLE must be positive")
	}
	if c.MinSize < 0 {
		return fmt.Errorf("PC_COLD_TIER_MIN_SIZE cannot be negative")
	}
	if c.Batch <= 0 {
		return fmt.Errorf("PC_COLD_TIER_BATCH must be positive")
	}
	return nil
}
