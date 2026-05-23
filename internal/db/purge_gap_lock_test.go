//go:build !mutest

package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// lockWaitBudget bounds the neighbor insert so a lock wait fails the test
// rather than hanging it. InnoDB's own innodb_lock_wait_timeout is 50s.
const lockWaitBudget = 6 * time.Second

// A tenant purge deletes by tenant_id across many tables. Under MySQL's default
// REPEATABLE READ that range delete gap-locks the gaps its scan crosses, and
// clearing one tenant's rows merges the gap so it covers where a neighboring
// tenant's insert would go. An unrelated tenant's write then blocks behind the
// purge for as long as it runs.
//
// READ COMMITTED takes no gap locks for a range scan, so the neighbor's insert
// goes through while the purge is still open.
func TestBeginIsolated_PurgeDoesNotBlockAnotherTenantsInsert(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT != mysql; only MySQL gap-locks a range delete by default")
	}
	pool := testdb.MySQL(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `CREATE TABLE gap_probe (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		tenant_id VARCHAR(64) NOT NULL,
		KEY idx_gap_probe_tenant (tenant_id)
	) ENGINE=InnoDB`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS gap_probe") })

	// Three tenants in index order. The purge clears the middle one, which
	// merges the gap either side of it.
	for i := 0; i < 40; i++ {
		for _, tn := range []string{"aaa_first", "bbb_purged", "ccc_last"} {
			if _, err := pool.Exec(ctx, "INSERT INTO gap_probe (id, tenant_id) VALUES (?, ?)",
				fmt.Sprintf("%s-%d", tn, i), tn); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}

	isolated, ok := pool.(interface {
		BeginIsolated(context.Context, sql.IsolationLevel) (*sql.Tx, error)
	})
	if !ok {
		t.Fatal("pool does not offer BeginIsolated, so the purge cannot ask for READ COMMITTED")
	}

	purge, err := isolated.BeginIsolated(ctx, sql.LevelReadCommitted)
	if err != nil {
		t.Fatalf("begin purge tx: %v", err)
	}
	defer purge.Rollback() //nolint:errcheck // the insert is what is under test

	if _, err := purge.ExecContext(ctx, "DELETE FROM gap_probe WHERE tenant_id = ?", "bbb_purged"); err != nil {
		t.Fatalf("purge delete: %v", err)
	}

	// With the purge still open, a neighboring tenant inserts into the gap it
	// would have locked. Bounded so a block fails the test instead of hanging.
	done := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		insertCtx, cancel := context.WithTimeout(ctx, lockWaitBudget)
		defer cancel()
		_, err := pool.Exec(insertCtx,
			"INSERT INTO gap_probe (id, tenant_id) VALUES (?, ?)", "aaa_first-neighbor", "aaa_first")
		done <- err
	}()
	wg.Wait()

	if err := <-done; err != nil {
		t.Fatalf("an unrelated tenant's insert was blocked by the open purge: %v", err)
	}
}
