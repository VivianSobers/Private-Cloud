// Command cloudctl is the server-side admin CLI.
//
// It exists mainly as the lockout escape hatch. Passkeys have no "forgot
// password" flow, so without a way to clear credentials from the server, a
// lost authenticator plus lost recovery codes would mean a permanently
// inaccessible account.
//
// This does not weaken the security model: it requires shell access to the
// server, and anyone with that already has the database and the files.
//
// The full command list lives in usage() rather than being duplicated here,
// where it drifted out of date once already: this comment listed neither
// app-password, nor migrate-blobs, nor any of the jobs subcommands. One place to
// read, one place to update.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/blob"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/cas"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/extract"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/files"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/jobs"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/shares"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cloudctl — private cloud admin

  user list                       list all users
  user create <username> [--admin]  create a user and print recovery codes
  user app-password <username> <name> [--ttl-hours=N]  mint an app password (headless setup)
  user reset-auth <username>      remove all passkeys, revoke sessions, reissue codes
  user disable <username>         soft-lock an account
  user enable <username>          unlock an account
  recovery regenerate <username>  issue a fresh set of recovery codes
  cleanup                         purge expired sessions and ceremonies
  fsck [--repair]                 compare the blob store against the database
  gc                              purge expired trash and unreferenced blobs
  migrate-blobs [--limit=N] [--all]  rewrite Phase 1 whole-file blobs as chunks
  jobs stats                      show background job queue depth by state
  jobs failed [--limit=N]         list dead-lettered jobs and their errors
  jobs retry [--kind=KIND]        requeue dead-lettered jobs
  jobs reindex                    enqueue extraction for every live file

Requires PC_DATABASE_URL in the environment.
fsck, gc and migrate-blobs also require PC_BLOB_PATH.
gc also honours PC_TRASH_RETENTION, PC_KEEP_VERSIONS, PC_VERSION_RETENTION,
PC_CHANGE_RETENTION and PC_UPLOAD_TTL, and prints the policy it applies.
`)
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		return fmt.Errorf("no command given")
	}

	dsn := os.Getenv("PC_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("PC_DATABASE_URL is not set")
	}

	// Most commands are a handful of queries and should not hang forever if the
	// database is wedged. fsck and gc walk the whole blob store, which on a
	// full pool is minutes of I/O, so they get their own budget instead of
	// being killed halfway through a scan that reports nothing useful.
	timeout := 60 * time.Second
	switch args[0] {
	case "fsck", "gc", "migrate-blobs":
		timeout = 6 * time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Warnings only: routine CLI output should not be buried in info logs.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	database, err := db.Open(ctx, dsn, 4, 1, 10*time.Second, log)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()

	store := auth.NewStore(database.Pool)

	// WebAuthn config is irrelevant to every command here, but Service owns
	// recovery-code generation. Placeholder values keep the constructor happy
	// without pretending this process serves HTTP.
	svc, err := auth.NewService(store, auth.Config{
		RPDisplayName: "Private Cloud",
		RPID:          "localhost",
		RPOrigins:     []string{"http://localhost"},
		SessionTTL:    time.Hour,
	}, log)
	if err != nil {
		return err
	}

	switch args[0] {
	case "user":
		return userCommand(ctx, store, svc, args[1:])
	case "recovery":
		return recoveryCommand(ctx, store, svc, args[1:])
	case "cleanup":
		sessions, ceremonies, err := store.Cleanup(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("removed %d expired sessions, %d stale ceremonies\n", sessions, ceremonies)
		return nil
	case "fsck":
		return fsckCommand(ctx, database, log, args[1:])
	case "gc":
		return gcCommand(ctx, database, log)
	case "migrate-blobs":
		return migrateCommand(ctx, database, log, args[1:])
	case "jobs":
		return jobsCommand(ctx, database, args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func userCommand(ctx context.Context, store *auth.Store, svc *auth.Service, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cloudctl user <list|create|app-password|reset-auth|disable|enable>")
	}

	switch args[0] {
	case "list":
		users, err := store.ListUsers(ctx)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Println("no users yet — open the web UI to create the first admin")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "USERNAME\tADMIN\tSTATUS\tPASSKEYS\tCODES LEFT\tCREATED")
		for _, u := range users {
			creds, _ := store.ListCredentials(ctx, u.ID)
			codes, _ := store.CountUnusedRecoveryCodes(ctx, u.ID)

			status := "active"
			if u.Disabled() {
				status = "DISABLED"
			}
			// A user with zero passkeys is one bad day from being locked out.
			passkeys := fmt.Sprintf("%d", len(creds))
			if len(creds) == 0 {
				passkeys = "0 (!)"
			}
			fmt.Fprintf(w, "%s\t%t\t%s\t%s\t%d\t%s\n",
				u.Username, u.IsAdmin, status, passkeys, codes, u.CreatedAt.Format("2006-01-02"))
		}
		return w.Flush()

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: cloudctl user create <username> [--admin]")
		}
		username := args[1]
		isAdmin := len(args) > 2 && args[2] == "--admin"

		if strings.TrimSpace(username) == "" || strings.ContainsAny(username, " \t/\\") {
			return fmt.Errorf("username must be non-empty and contain no spaces or slashes")
		}

		// Id is minted here rather than by the database, because the bootstrap
		// registration path needs the same UUID to exist before the row does
		// (it becomes the WebAuthn user handle). One code path, one rule.
		user, err := store.CreateUser(ctx, uuid.New(), username, username, isAdmin)
		if err != nil {
			return err
		}

		// The new user has no passkey yet. Recovery codes are how they get in
		// the first time: redeem one, then enrol a passkey. That reuses the
		// recovery path instead of inventing a separate invite-token concept.
		codes, err := svc.RegenerateRecoveryCodes(ctx, user.ID)
		if err != nil {
			return err
		}

		fmt.Printf("created user %q (admin: %t)\n\n", user.Username, user.IsAdmin)
		printCodes(codes)
		fmt.Printf("\nThey sign in by redeeming one code, then registering a passkey.\n")
		return nil

	case "reset-auth":
		if len(args) < 2 {
			return fmt.Errorf("usage: cloudctl user reset-auth <username>")
		}
		user, err := store.GetUserByUsername(ctx, args[1])
		if err != nil {
			return err
		}

		// All three together, deliberately. Clearing passkeys while leaving
		// live sessions running would be a half-measure, and leaving the old
		// recovery codes valid would leave the previous holder a way back in.
		removed, err := store.DeleteAllCredentials(ctx, user.ID)
		if err != nil {
			return err
		}
		revoked, err := store.RevokeAllSessions(ctx, user.ID)
		if err != nil {
			return err
		}
		// App passwords too. An account being recovered must not leave a
		// working WebDAV credential behind for whoever caused the lockout.
		apps, err := store.RevokeAllAppPasswords(ctx, user.ID)
		if err != nil {
			return err
		}
		codes, err := svc.RegenerateRecoveryCodes(ctx, user.ID)
		if err != nil {
			return err
		}

		fmt.Printf("reset %q: removed %d passkey(s), revoked %d session(s), revoked %d app password(s)\n\n",
			user.Username, removed, revoked, apps)
		printCodes(codes)
		return nil

	case "disable", "enable":
		if len(args) < 2 {
			return fmt.Errorf("usage: cloudctl user %s <username>", args[0])
		}
		user, err := store.GetUserByUsername(ctx, args[1])
		if err != nil {
			return err
		}
		disable := args[0] == "disable"
		if err := store.SetUserDisabled(ctx, user.ID, disable); err != nil {
			return err
		}
		if disable {
			// Otherwise the account is "disabled" but its existing sessions
			// keep working until they expire.
			revoked, err := store.RevokeAllSessions(ctx, user.ID)
			if err != nil {
				return err
			}
			fmt.Printf("disabled %q and revoked %d session(s)\n", user.Username, revoked)
		} else {
			fmt.Printf("enabled %q\n", user.Username)
		}
		return nil

	case "app-password":
		// Mint an app password from the CLI — the headless path for setting up the
		// sync client or a WebDAV mount on a box with no browser to reach the web UI.
		if len(args) < 3 {
			return fmt.Errorf("usage: cloudctl user app-password <username> <name> [--ttl-hours=N]")
		}
		user, err := store.GetUserByUsername(ctx, args[1])
		if err != nil {
			return err
		}
		var ttl time.Duration
		for _, a := range args[3:] {
			if strings.HasPrefix(a, "--ttl-hours=") {
				n, err := strconv.Atoi(strings.TrimPrefix(a, "--ttl-hours="))
				if err != nil || n < 0 {
					return fmt.Errorf("--ttl-hours must be a non-negative integer")
				}
				ttl = time.Duration(n) * time.Hour
			}
		}
		ap, plaintext, err := store.CreateAppPassword(ctx, user.ID, args[2], ttl)
		if err != nil {
			return err
		}
		fmt.Printf("app password %q created for %q\n", ap.Name, user.Username)
		if ap.ExpiresAt != nil {
			fmt.Printf("expires: %s\n", ap.ExpiresAt.Format("2006-01-02 15:04"))
		}
		fmt.Printf("\n    %s\n\n", plaintext)
		fmt.Println("Copy it now — it is shown once and cannot be retrieved.")
		return nil

	default:
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
}

func recoveryCommand(ctx context.Context, store *auth.Store, svc *auth.Service, args []string) error {
	if len(args) < 2 || args[0] != "regenerate" {
		return fmt.Errorf("usage: cloudctl recovery regenerate <username>")
	}

	user, err := store.GetUserByUsername(ctx, args[1])
	if err != nil {
		return err
	}
	codes, err := svc.RegenerateRecoveryCodes(ctx, user.ID)
	if err != nil {
		return err
	}

	fmt.Printf("new recovery codes for %q (previous codes are now invalid)\n\n", user.Username)
	printCodes(codes)
	return nil
}

// --- storage ----------------------------------------------------------------

// filesService builds the files service the storage commands need. Kept
// separate from the auth wiring above because fsck has no business opening a
// WebAuthn configuration.
func filesService(database *db.DB, log *slog.Logger) (*files.Service, error) {
	path := os.Getenv("PC_BLOB_PATH")
	if path == "" {
		return nil, fmt.Errorf("PC_BLOB_PATH is not set")
	}
	store, err := blob.NewFSStore(path)
	if err != nil {
		return nil, err
	}
	svc := files.NewService(files.NewStore(database.Pool), store, log)

	// Without this, fsck sees chunks as orphans and --repair deletes every
	// deduplicated byte in the system.
	casStore, err := cas.NewStore(database.Pool, store)
	if err != nil {
		return nil, fmt.Errorf("content-addressed store: %w", err)
	}
	svc.SetCAS(casStore)

	return svc, nil
}

func fsckCommand(ctx context.Context, database *db.DB, log *slog.Logger, args []string) error {
	repair := len(args) > 0 && args[0] == "--repair"

	svc, err := filesService(database, log)
	if err != nil {
		return err
	}

	report, err := svc.Fsck(ctx, repair)
	if err != nil {
		return err
	}

	fmt.Printf("checked %d file(s) on disk (%d of them chunks)\n", report.Checked, report.ChunksChecked)
	fmt.Printf("orphans (on disk, no database row): %d (%s)\n",
		len(report.Orphans), humanBytes(report.OrphanBytes))

	// Missing content is the one finding that is not self-healing. Print every
	// key rather than a count, because the response is a restore from backup
	// and that needs to know exactly what to look for.
	if len(report.Missing) > 0 {
		fmt.Printf("\nMISSING CONTENT — %d file(s) have database rows but no bytes.\n", len(report.Missing))
		fmt.Println("This is data loss. Restore from a snapshot or from restic; see")
		fmt.Println("docs/runbook-restore.md.")
		for _, k := range report.Missing {
			fmt.Printf("    %s\n", k)
		}
	}
	if len(report.MissingChunks) > 0 {
		fmt.Printf("\nMISSING CHUNKS — %d chunk(s) have rows but no bytes.\n", len(report.MissingChunks))
		fmt.Println("Chunks are SHARED: each missing one damages every file that")
		fmt.Println("references it, possibly across users. Restore from backup.")
		for _, k := range report.MissingChunks {
			fmt.Printf("    %s\n", k)
		}
	}
	if len(report.RefcountDrift) > 0 {
		fmt.Printf("\nREFCOUNT DRIFT — %d chunk(s) disagree with reality:\n", len(report.RefcountDrift))
		fmt.Println("The maintenance trigger did not do its job. Not corrected here:")
		fmt.Println("an inflated count only wastes disk, but a deflated one gets live")
		fmt.Println("chunks deleted, and overwriting it would destroy the evidence.")
		for _, d := range report.RefcountDrift {
			fmt.Printf("    %s\n", d)
		}
	}
	if len(report.SizeMismatch) > 0 {
		fmt.Printf("\nSIZE MISMATCH — %d blob(s) disagree with their recorded size:\n", len(report.SizeMismatch))
		for _, m := range report.SizeMismatch {
			fmt.Printf("    %s\n", m)
		}
	}

	if repair {
		fmt.Printf("\nrepaired: removed %d orphan(s) and %d leftover temp file(s)\n",
			len(report.Orphans), report.TempFilesRemoved)
	} else if len(report.Orphans) > 0 {
		fmt.Println("\nrun `cloudctl fsck --repair` to delete the orphans")
	}

	// Non-zero exit on data loss so a cron wrapper notices without parsing text.
	if len(report.Missing) > 0 || len(report.MissingChunks) > 0 ||
		len(report.SizeMismatch) > 0 || len(report.RefcountDrift) > 0 {
		return fmt.Errorf("fsck found %d missing blob(s), %d missing chunk(s), %d mismatch(es), %d refcount drift(s)",
			len(report.Missing), len(report.MissingChunks),
			len(report.SizeMismatch), len(report.RefcountDrift))
	}
	return nil
}

func gcCommand(ctx context.Context, database *db.DB, log *slog.Logger) error {
	svc, err := filesService(database, log)
	if err != nil {
		return err
	}
	// Every retention knob the API honours, not just one.
	//
	// Only PC_TRASH_RETENTION was read here, so the others silently fell back to
	// the NewService defaults. An operator who set PC_KEEP_VERSIONS=100 in .env
	// and then ran `cloudctl gc` by hand got pruning at 25 — the command
	// destroying history the running server was configured to keep, with nothing
	// in its output to say so.
	for _, k := range []struct {
		env string
		set func(time.Duration)
	}{
		{"PC_TRASH_RETENTION", func(d time.Duration) { svc.TrashRetention = d }},
		{"PC_VERSION_RETENTION", func(d time.Duration) { svc.VersionRetention = d }},
		{"PC_CHANGE_RETENTION", func(d time.Duration) { svc.ChangeRetention = d }},
		{"PC_UPLOAD_TTL", func(d time.Duration) { svc.UploadTTL = d }},
	} {
		v := os.Getenv(k.env)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s: %w", k.env, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s must be positive, got %q", k.env, v)
		}
		k.set(d)
	}
	if v := os.Getenv("PC_KEEP_VERSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("PC_KEEP_VERSIONS must be an integer of at least 1, got %q", v)
		}
		svc.KeepVersions = n
	}

	// Say what policy this run is about to apply, since it decides what gets
	// deleted and the defaults are not obvious from the command line.
	fmt.Printf("policy: trash %s, versions keep %d / %s, journal %s, uploads %s\n",
		svc.TrashRetention, svc.KeepVersions, svc.VersionRetention,
		svc.ChangeRetention, svc.UploadTTL)

	sharesSvc := shares.NewService(shares.NewStore(database.Pool), svc, log)
	staleShares, err := sharesSvc.CollectStale(ctx)
	if err != nil {
		return err
	}

	res, err := svc.CollectGarbage(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("purged %d expired trash item(s)\n", res.TrashPurged)
	fmt.Printf("reclaimed %d stale share(s)\n", staleShares)
	fmt.Printf("pruned %d old version(s), %d journal entr(ies)\n", res.VersionsPruned, res.ChangesPruned)
	fmt.Printf("freed %d blob(s), %s\n", res.BlobsFreed, humanBytes(res.BytesFreed))
	fmt.Printf("expired %d abandoned upload(s), swept %d staging file(s)\n",
		res.UploadsExpired, res.StagingFreed)
	fmt.Printf("freed %d manifest(s), %d chunk(s), %s\n",
		res.ManifestsFreed, res.ChunksFreed, humanBytes(res.ChunkBytesFreed))
	fmt.Printf("pruned %d extracted text row(s), %d embedding(s)\n",
		res.DocTextPruned, res.EmbeddingsPruned)
	return nil
}

// migrateCommand drains Phase 1 whole-file blobs into content-addressed chunks.
//
// Operator-driven on purpose: a storage rewrite is exactly when a fresh backup
// should exist first (see docs/phase-2-design.md §0), so this is a command the
// admin runs deliberately rather than something the server does on its own.
// One bounded batch by default; --all repeats until the backlog is drained.
func migrateCommand(ctx context.Context, database *db.DB, log *slog.Logger, args []string) error {
	limit := 100
	all := false
	for _, a := range args {
		switch {
		case a == "--all":
			all = true
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil || n <= 0 {
				return fmt.Errorf("--limit must be a positive integer")
			}
			limit = n
		default:
			return fmt.Errorf("usage: cloudctl migrate-blobs [--limit=N] [--all]")
		}
	}

	svc, err := filesService(database, log)
	if err != nil {
		return err
	}

	var total files.MigrateResult
	for {
		res, err := svc.MigrateBlobs(ctx, limit)
		if err != nil {
			return err
		}
		total.VersionsMigrated += res.VersionsMigrated
		total.BytesWritten += res.BytesWritten
		total.Deduped += res.Deduped
		total.Skipped += res.Skipped
		total.Failed += res.Failed

		// Stop when a batch makes no forward progress. That both ends the single
		// -batch run and guarantees --all terminates: versions that only ever fail
		// (missing bytes, size disagreement) stay blob-backed and would otherwise
		// reappear every batch forever.
		if !all || res.VersionsMigrated == 0 {
			break
		}
	}

	fmt.Printf("migrated %d version(s), wrote %s of new chunks\n",
		total.VersionsMigrated, humanBytes(total.BytesWritten))
	fmt.Printf("deduped %d, skipped %d (raced), failed %d\n",
		total.Deduped, total.Skipped, total.Failed)
	if total.Failed > 0 {
		// Non-zero exit so a wrapper notices without parsing text: a failure here
		// is a version whose bytes are missing or disagree with their row — an
		// integrity anomaly for fsck, not a transient the next run clears.
		return fmt.Errorf("%d version(s) could not be migrated; run cloudctl fsck", total.Failed)
	}
	return nil
}

// jobsCommand inspects and repairs the background job queue — the operator side
// of the metrics and the JobsDeadLettering alert.
func jobsCommand(ctx context.Context, database *db.DB, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cloudctl jobs <stats|failed|retry|reindex>")
	}
	store := jobs.NewStore(database.Pool)

	switch args[0] {
	case "stats":
		stats, err := store.Stats(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "STATE\tCOUNT")
		for _, s := range []string{jobs.StateQueued, jobs.StateRunning, jobs.StateDone, jobs.StateFailed} {
			fmt.Fprintf(w, "%s\t%d\n", s, stats[s])
		}
		return w.Flush()

	case "failed":
		limit := 100
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "--limit=") {
				n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
				if err != nil || n <= 0 {
					return fmt.Errorf("--limit must be a positive integer")
				}
				limit = n
			}
		}
		failed, err := store.ListFailed(ctx, limit)
		if err != nil {
			return err
		}
		if len(failed) == 0 {
			fmt.Println("no failed jobs")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "KIND\tNODE\tATTEMPTS\tCREATED\tERROR")
		for _, j := range failed {
			node := "-"
			if j.NodeID != nil {
				node = j.NodeID.String()[:8]
			}
			fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\n",
				j.Kind, node, j.Attempts, j.MaxAttempts,
				j.CreatedAt.Format("2006-01-02 15:04"), truncate(j.LastError, 80))
		}
		return w.Flush()

	case "retry":
		// Optional kind filter: the reasons jobs fail are per-kind, so requeuing
		// everything after fixing one cause re-runs work that failed for another.
		var kind string
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "--kind=") {
				kind = strings.TrimPrefix(a, "--kind=")
			}
		}
		n, err := store.RetryFailed(ctx, kind)
		if err != nil {
			return err
		}
		if kind != "" {
			fmt.Printf("requeued %d failed %s job(s)\n", n, kind)
		} else {
			fmt.Printf("requeued %d failed job(s)\n", n)
		}
		return nil

	case "reindex":
		// Enqueue extraction for every live file — rebuilds text (and, if the chain
		// is on, embeddings) after an extractor or model change. Content-addressed
		// and idempotent, so this is safe to run any time; the worker drains it.
		n, err := store.EnqueueForAllFiles(ctx, extract.Kind)
		if err != nil {
			return err
		}
		fmt.Printf("enqueued %d extraction job(s) for re-indexing\n", n)
		return nil

	default:
		return fmt.Errorf("unknown jobs subcommand %q (want stats, failed, retry or reindex)", args[0])
	}
}

// truncate shortens a string for single-line tabular output.
func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func printCodes(codes []string) {
	fmt.Println("RECOVERY CODES — shown once, stored only as hashes:")
	fmt.Println()
	for _, c := range codes {
		fmt.Printf("    %s\n", c)
	}
	fmt.Println()
	fmt.Println("Print these and keep them with the ZFS passphrase. There is no way")
	fmt.Println("to retrieve them later — only to regenerate, which invalidates these.")
}
