package plugintest

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// HoldDatabasePreparation stops the harness from preparing test databases
// until the test ends. The harness prepares MySQL and SQL Server databases in
// the background while tests run, and a goroutine leak check that counts
// goroutines after taking its host reads that work as the plugin's leak. Call
// it once the host is in hand and before the check takes its baseline. A
// preparation already running finishes first, and a test that holds
// preparation cannot take another database.
func HoldDatabasePreparation(t testing.TB) {
	t.Helper()
	testdb.HoldPreparation(t)
}
