// Package testdb gives each integration test binary its own database.
//
// The suite used to run every package against whatever database
// PC_TEST_DATABASE_URL named, and share it. That worked exactly once per
// database: a second run met the first run's rows, and the failures it produced
// looked like regressions rather than like leftovers. Three of them were
// particularly good at wasting an afternoon —
//
//   - chunk GC counts chunks on disk globally, so a chunk another fixture left
//     behind is one this fixture is asked to explain;
//   - media variants are keyed by content hash, so a thumbnail rendered by an
//     earlier run belongs to a blob store that has since been deleted, and the
//     variant "exists" with no bytes behind it;
//   - the extract pipeline dedupes by (kind, node) on a unique partial index, so
//     an identical upload enqueues nothing the second time.
//
// None of those is a bug. All three are the same bug: the test's idea of "empty"
// was a database nobody had emptied. The documented workaround was to recreate
// the Postgres container between runs and pass -p 1, which is a real cost paid
// on every single run to avoid a fixed cost paid once, here.
//
// Main creates a database per test binary, points the environment at it, and
// drops it afterwards. Packages opt in from TestMain; fixtures are untouched,
// because they still read PC_TEST_DATABASE_URL and it still names a database
// they may migrate and fill. What changes is that the database is theirs.
//
// This also makes the suite parallel-safe across packages, so -p 1 is no longer
// required for isolation. It is still worth passing when a machine is small:
// each package holds its own pool.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvVar is the variable fixtures read to find their database.
const EnvVar = "PC_TEST_DATABASE_URL"

// M is the subset of *testing.M this package needs. Taking an interface keeps
// the testing package out of a non-test file.
type M interface{ Run() int }

// Main creates a scratch database, repoints EnvVar at it for the duration of
// the run, and drops it when the tests finish. The label is only there to make
// a leaked database name say which package leaked it.
//
// With EnvVar unset it runs the tests unchanged: the integration tests skip
// themselves, and the unit tests in the same package must still run.
func Main(m M, label string) int {
	admin := os.Getenv(EnvVar)
	if admin == "" {
		return m.Run()
	}

	name, dsn, drop, err := create(admin, label)
	if err != nil {
		// Failing loudly beats silently running against the admin database,
		// which is the state this package exists to prevent.
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		return 1
	}
	defer drop()

	if err := os.Setenv(EnvVar, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: set %s: %v\n", EnvVar, err)
		return 1
	}
	code := m.Run()
	_ = name
	return code
}

func create(adminDSN, label string) (name, dsn string, drop func(), err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Random rather than derived from the label: two runs of the same package
	// against one server must not collide, and a database leaked by a killed
	// run must not block the next one.
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", nil, fmt.Errorf("random suffix: %w", err)
	}
	name = "pctest_" + sanitise(label) + "_" + hex.EncodeToString(suffix[:])

	pool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return "", "", nil, fmt.Errorf("connect to %s: %w", EnvVar, err)
	}
	defer pool.Close()

	// CREATE DATABASE cannot run inside a transaction. The identifier is
	// generated here from a fixed alphabet, so there is nothing to inject.
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return "", "", nil, fmt.Errorf("create database %s: %w", name, err)
	}

	u, err := url.Parse(adminDSN)
	if err != nil {
		return "", "", nil, fmt.Errorf("parse %s: %w", EnvVar, err)
	}
	u.Path = "/" + name

	drop = func() {
		// A fresh connection: the pool above is closed by the deferred Close,
		// and the caller's pools are closed by their own cleanups.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		p, err := pgxpool.New(ctx, adminDSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testdb: reconnect to drop %s: %v\n", name, err)
			return
		}
		defer p.Close()

		// WITH (FORCE) terminates leftover backends. Without it a single
		// connection a pool has not finished closing makes the DROP wait until
		// the binary exits, leaking a database per run — which is the failure
		// this package exists to stop, arriving by a different door.
		if _, err := p.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			fmt.Fprintf(os.Stderr, "testdb: drop %s: %v\n", name, err)
		}
	}
	return name, u.String(), drop, nil
}

// sanitise reduces a label to the identifier alphabet, so a caller cannot turn
// a package name into SQL.
func sanitise(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "pkg"
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return string(out)
}
