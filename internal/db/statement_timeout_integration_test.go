//go:build !mutest

package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// TestStatementTimeout_PG_KillsLongQuery verifies that the default 30s
// statement_timeout from DSN parameter actually cancels a long-running
// query on PostgreSQL. Confirms the DSN approach works end-to-end.
func TestStatementTimeout_PG_KillsLongQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("postgres not in CI_DIALECT")
	}

	d := testdb.Postgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// pg_sleep(35) exceeds the default 30s statement_timeout set via DSN.
	_, err := d.Exec(ctx, "SELECT pg_sleep(35)")
	if err == nil {
		t.Fatal("expected statement_timeout to cancel pg_sleep(35), got nil")
	}
	if !strings.Contains(err.Error(), "canceling statement due to statement timeout") {
		t.Fatalf("expected PG statement timeout error, got: %v", err)
	}
}
