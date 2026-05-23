//go:build !mutest

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// The check reports and never acts. ALTER DATABASE ... SET
// READ_COMMITTED_SNAPSHOT waits for exclusive access to the database and no
// lock timeout applies to it, so running it at boot against a database serving
// traffic blocks until every other session disconnects, and the WITH ROLLBACK
// IMMEDIATE form returns promptly only by killing those sessions. Either would
// be worse than the deadlocks it prevents, so the operator picks the window.
func TestCheckSnapshotReads_ReportsWithoutActing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT != mssql")
	}
	pool := testdb.MSSQL(t)
	ctx := context.Background()

	ok, remedy, err := db.CheckSnapshotReads(ctx, pool)
	if err != nil {
		t.Fatalf("CheckSnapshotReads: %v", err)
	}
	if ok {
		if remedy != "" {
			t.Errorf("nothing to do, but a remedy was returned: %q", remedy)
		}
		return
	}
	if remedy == "" {
		t.Fatal("snapshot reads are off but no remedy was named")
	}
	if !strings.Contains(remedy, "READ_COMMITTED_SNAPSHOT ON") {
		t.Errorf("remedy does not name the setting: %q", remedy)
	}

	// The setting must still be off: reporting it must not have changed it.
	after, _, err := db.CheckSnapshotReads(ctx, pool)
	if err != nil {
		t.Fatalf("CheckSnapshotReads again: %v", err)
	}
	if after {
		t.Error("the check enabled snapshot reads; it must only report")
	}
}

// Postgres and MySQL serve reads from a snapshot already, so the check is a
// no-op there and must never suggest SQL Server DDL to their operators.
func TestCheckSnapshotReads_NoOpOffMSSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, name := range []string{"postgres", "mysql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			pool := testdb.Postgres(t)
			if name == "mysql" {
				pool = testdb.MySQL(t)
			}
			ok, remedy, err := db.CheckSnapshotReads(context.Background(), pool)
			if err != nil {
				t.Fatalf("CheckSnapshotReads: %v", err)
			}
			if !ok || remedy != "" {
				t.Errorf("want no-op on %s, got ok=%v remedy=%q", name, ok, remedy)
			}
		})
	}
}
