// Package testdialect says which database dialects a test run covers.
//
// CI_DIALECT names the one dialect a run covers (postgres, mysql or mssql), and
// a run with it unset covers all three. A test asks before it starts a server,
// so a run limited to one dialect starts that dialect's server alone. The CI
// matrix gives each dialect its own job for that reason, and on a machine that
// other runs share, a second dialect's server takes the memory whose absence
// makes SQL Server abort mid-suite.
//
// The package imports nothing from the engine, so the database package's own
// tests can use it. testdb imports the database package, which therefore cannot
// import testdb back.
package testdialect

import (
	"os"
	"testing"
)

// MSSQLImage is the SQL Server image every test starts, pinned by the digest
// of its multi-arch index. A moved tag changed startup time and the recovery
// line the tests wait on with no change here, so the pin moves only by an
// edit to this line.
const MSSQLImage = "mcr.microsoft.com/azure-sql-edge@sha256:902628a8be89e35dfb7895ca31d602974c7bafde4d583a0d0873844feb1c42cf"

// Enabled reports whether this run covers the named dialect.
func Enabled(name string) bool {
	d := os.Getenv("CI_DIALECT")
	return d == "" || d == name
}

// Require skips the test when this run does not cover the named dialect. Call
// it before starting that dialect's server.
func Require(t testing.TB, name string) {
	t.Helper()
	if !Enabled(name) {
		t.Skipf("CI_DIALECT=%s does not cover %s", os.Getenv("CI_DIALECT"), name)
	}
}
