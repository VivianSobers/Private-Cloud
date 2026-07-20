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
//	cloudctl user list
//	cloudctl user create <username> [--admin]
//	cloudctl user reset-auth <username>
//	cloudctl user disable <username>
//	cloudctl user enable <username>
//	cloudctl recovery regenerate <username>
//	cloudctl cleanup
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/auth"
	"github.com/guru-bharadwaj20/private-cloud/server/internal/db"
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
  user reset-auth <username>      remove all passkeys, revoke sessions, reissue codes
  user disable <username>         soft-lock an account
  user enable <username>          unlock an account
  recovery regenerate <username>  issue a fresh set of recovery codes
  cleanup                         purge expired sessions and ceremonies

Requires PC_DATABASE_URL in the environment.
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		return fmt.Errorf("usage: cloudctl user <list|create|reset-auth|disable|enable>")
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
		codes, err := svc.RegenerateRecoveryCodes(ctx, user.ID)
		if err != nil {
			return err
		}

		fmt.Printf("reset %q: removed %d passkey(s), revoked %d session(s)\n\n", user.Username, removed, revoked)
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
