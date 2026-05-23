//go:build integration
// +build integration

package tests

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PluginMigrate handles concurrent boot without a duplicate-key crash. Two
// processes booting at once must both succeed and record the version once: a
// plain INSERT of the version row would fail with error 23505 for the second.
func TestPluginMigrate_ConcurrentBoot(t *testing.T) {
	ctx := context.Background()

	// Start PostgreSQL container
	ctr, err := postgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:16-alpine"),
		postgres.WithDatabase("test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ctr.Terminate(ctx)

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	// Migration files with one script
	// Use a unique table name per run so concurrent tests don't collide.
	tableName := fmt.Sprintf("plugin_concurrent_migrate_%d", time.Now().UnixNano())
	migrationsFS := fstest.MapFS{
		"psql/001_create_test_table.up.sql": &fstest.MapFile{
			Data: []byte(fmt.Sprintf(
				"CREATE TABLE IF NOT EXISTS %s_concurrent_data (id SERIAL PRIMARY KEY, value TEXT)",
				tableName,
			)),
		},
	}

	// Run concurrent boots
	const concurrency = 10
	var (
		wg       sync.WaitGroup
		errCount atomic.Int32
	)

	for i := range concurrency {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Each goroutine opens its own connection to simulate
			// independent plugin processes booting against the same DB.
			pool, err := openDB("pgx", connStr)
			if err != nil {
				t.Errorf("goroutine %d: open db: %v", idx, err)
				errCount.Add(1)
				return
			}
			defer pool.Close()

			// Small stagger to maximize the race window
			time.Sleep(time.Duration(idx) * 20 * time.Millisecond)

			mErr := core.PluginMigrate(ctx, pool, "postgres", migrationsFS, tableName)
			if mErr != nil {
				t.Errorf("goroutine %d: PluginMigrate failed: %v", idx, mErr)
				errCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	assert.Zero(t, errCount.Load(), "all PluginMigrate calls should succeed on concurrent boot")
}

// TestPluginMigrate_LockPreventsDoubleApply verifies that the
// INSERT-dedup lock prevents concurrent PluginMigrate calls from executing
// the same migration SQL more than once. A migration that inserts a row as a
// side-effect must produce exactly one row: not one per goroutine.
func TestPluginMigrate_LockPreventsDoubleApply(t *testing.T) {
	ctx := context.Background()

	ctr, err := postgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:16-alpine"),
		postgres.WithDatabase("test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ctr.Terminate(ctx)

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	prefix := fmt.Sprintf("miglock_%d", time.Now().UnixNano())
	trackTable := prefix + "_track"
	sideTable := prefix + "_side"

	// Migration is deliberately non-idempotent in its side-effect: each
	// execution inserts a row. If the lock fails, sideTable has >1 row.
	migrationsFS := fstest.MapFS{
		"psql/001_seed.up.sql": &fstest.MapFile{
			Data: []byte(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (id SERIAL PRIMARY KEY, ts TIMESTAMPTZ DEFAULT NOW());
INSERT INTO %s DEFAULT VALUES;
`, sideTable, sideTable)),
		},
	}

	const concurrency = 10
	var (
		wg       sync.WaitGroup
		errCount atomic.Int32
	)

	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()

			db, openErr := openDB("pgx", connStr)
			if openErr != nil {
				t.Errorf("open db: %v", openErr)
				errCount.Add(1)
				return
			}
			defer db.Close()

			if mErr := core.PluginMigrate(ctx, db, "postgres", migrationsFS, trackTable); mErr != nil {
				t.Errorf("PluginMigrate: %v", mErr)
				errCount.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Zero(t, errCount.Load(), "all concurrent PluginMigrate calls should succeed")

	// Verify with a fresh connection.
	verifyDB, err := openDB("pgx", connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer verifyDB.Close()

	// Tracking table integrity: exactly one version, no duplicates.
	var versionCount int
	err = verifyDB.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", trackTable)).Scan(&versionCount)
	assert.NoError(t, err)
	assert.Equal(t, 1, versionCount, "tracking table should have exactly 1 version entry")

	// Lock proof: migration SQL executed exactly once.
	var dataCount int
	err = verifyDB.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", sideTable)).Scan(&dataCount)
	assert.NoError(t, err)
	assert.Equal(t, 1, dataCount, "migration SQL should execute exactly once (lock prevented double-apply)")
}

func openDB(driver, connStr string) (*sql.DB, error) {
	db, err := sql.Open(driver, connStr)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Second)
	return db, nil
}
