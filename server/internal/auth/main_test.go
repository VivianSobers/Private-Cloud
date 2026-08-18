package auth_test

import (
	"os"
	"testing"

	"github.com/guru-bharadwaj20/private-cloud/server/internal/testdb"
)

// TestMain gives this package's integration tests a database of their own.
//
// Without it they share whatever PC_TEST_DATABASE_URL names, which works once
// and then meets the previous run's rows. See internal/testdb for the three
// specific ways that surfaced as false regressions.
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, "auth"))
}
