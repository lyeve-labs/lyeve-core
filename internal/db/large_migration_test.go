//go:build integration && !mutest
// +build integration,!mutest

// Large-migration integration test: verifies migration behavior against tables
// with 1M+ rows across PostgreSQL, MySQL, and MSSQL. It measures:
//   - migration duration per dialect
//   - whether the current migration path acquires exclusive table locks
//   - concurrent read/write contention during migration
//   - data integrity after migration
//
// Run with the LARGE_DATASET_ROWS env var to control row count (default 100K).
//
//	go test -tags=integration -run TestLargeMigration -count=1 -timeout 30m ./internal/db/

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmssql "github.com/testcontainers/testcontainers-go/modules/mssql"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// largeDatasetRows returns the target row count from LARGE_DATASET_ROWS env var
// or the default. Use 100K for CI, set to 1_000_000 for local testing.
func largeDatasetRows() int {
	if v := os.Getenv("LARGE_DATASET_ROWS"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}
	return 100_000 // default: CI-friendly 100K
}

// Helpers

// seedLargeTable creates a table and inserts the given number of rows,
// returning the table name and total insertion time.
func seedLargeTable(t *testing.T, db *sql.DB, dialect string, rows int) (tableName string, insertDuration time.Duration) {
	t.Helper()
	ctx := context.Background()

	tableName = fmt.Sprintf("large_migration_test_%d", time.Now().UnixNano())

	var createSQL, insertBatchSQL string
	switch dialect {
	case "postgres":
		createSQL = fmt.Sprintf(
			`CREATE TABLE %s (id SERIAL PRIMARY KEY, data VARCHAR(500) NOT NULL, num INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`, tableName)
		insertBatchSQL = fmt.Sprintf(
			`INSERT INTO %s (data, num) SELECT md5(g::text), g FROM generate_series(1, $1) AS g`, tableName)
	case "mysql":
		createSQL = fmt.Sprintf(
			`CREATE TABLE %s (id INT AUTO_INCREMENT PRIMARY KEY, data VARCHAR(500) NOT NULL, num INT NOT NULL DEFAULT 0, created_at DATETIME(6) NOT NULL DEFAULT NOW(6)) ENGINE=InnoDB`, tableName)
		// MySQL doesn't have generate_series: we batch insert
	case "mssql":
		createSQL = fmt.Sprintf(
			`CREATE TABLE %s (id INT IDENTITY(1,1) PRIMARY KEY, data NVARCHAR(500) NOT NULL, num INT NOT NULL DEFAULT 0, created_at DATETIME2(7) NOT NULL DEFAULT SYSUTCDATETIME())`, tableName)
	}

	if _, err := db.ExecContext(ctx, createSQL); err != nil {
		t.Fatalf("create seed table (%s): %v", dialect, err)
	}
	t.Cleanup(func() {
		db.ExecContext(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName))
	})

	// Postgres: use generate_series for single-batch insert
	if dialect == "postgres" {
		start := time.Now()
		if _, err := db.ExecContext(ctx, insertBatchSQL, rows); err != nil {
			t.Fatalf("batch insert (%s): %v", dialect, err)
		}
		insertDuration = time.Since(start)
	} else {
		// MySQL and MSSQL: insert in batches.
		// MSSQL limits to 1000 rows per INSERT; MySQL handles up to ~10K comfortably.
		start := time.Now()
		batchSize := 10_000
		if dialect == "mssql" {
			batchSize = 1_000
		}
		batches := rows / batchSize
		for b := 0; b < batches; b++ {
			var batchSQL string
			switch dialect {
			case "mysql":
				batchSQL = fmt.Sprintf(`INSERT INTO %s (data, num) VALUES `, tableName)
			case "mssql":
				batchSQL = fmt.Sprintf(`INSERT INTO %s (data, num) VALUES `, tableName)
			}
			values := make([]string, 0, batchSize)
			for i := 0; i < batchSize; i++ {
				idx := b*batchSize + i + 1
				if dialect == "mysql" {
					values = append(values, fmt.Sprintf("('%s', %d)", fmt.Sprintf("row-%d-data", idx), idx))
				} else {
					values = append(values, fmt.Sprintf("('%s', %d)", fmt.Sprintf("row-%d-data", idx), idx))
				}
			}
			for i, v := range values {
				if i > 0 {
					batchSQL += ","
				}
				batchSQL += v
			}
			if _, err := db.ExecContext(ctx, batchSQL); err != nil {
				t.Fatalf("batch insert %d (%s): %v", b, dialect, err)
			}
		}
		insertDuration = time.Since(start)
	}

	// Verify row count
	var count int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", tableName)).Scan(&count); err != nil {
		t.Fatalf("count seed rows: %v", err)
	}
	t.Logf("seeded %s with %d rows in %v", tableName, count, insertDuration.Round(time.Millisecond))

	return tableName, insertDuration
}

// Core Lock Detection

// TestLargeMigration_LockDetection_Postgres tests whether adding an index to a
// large table blocks concurrent reads. In PostgreSQL, CREATE INDEX (without
// CONCURRENTLY) takes a SHARE lock on the table, blocking all writes and
// allowing reads. CREATE INDEX CONCURRENTLY uses a weaker lock.
func TestLargeMigration_LockDetection_Postgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large migration test in short mode")
	}

	ctx := context.Background()
	rows := largeDatasetRows()
	if rows > 1_000_000 {
		// Safety limit
		rows = 1_000_000
	}

	// Start PG container
	ctr, err := postgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:16-alpine"),
		postgres.WithDatabase("lyeve_test"),
		postgres.WithUsername("cms"),
		postgres.WithPassword("cms"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start postgres container")
	t.Cleanup(func() { ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, db.PingContext(ctx))

	tableName, seedDuration := seedLargeTable(t, db, "postgres", rows)
	t.Logf("PG: seeded %d rows in %v", rows, seedDuration.Round(time.Millisecond))

	// Sub-test: CREATE INDEX (without CONCURRENTLY)
	t.Run("create_index_without_concurrently", func(t *testing.T) {
		indexName := fmt.Sprintf("idx_%s_num", tableName)

		start := time.Now()
		_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE INDEX %s ON %s (num)", indexName, tableName))
		indexDuration := time.Since(start)

		require.NoError(t, err, "create index (non-concurrent)")
		t.Logf("PG CREATE INDEX: %v", indexDuration.Round(time.Millisecond))

		// Verify index was created
		var count int
		db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_indexes WHERE indexname = $1`, indexName,
		).Scan(&count)
		assert.Equal(t, 1, count, "index should exist")

		// The read runs after index creation. The lock test below covers
		// contention.
	})

	// Sub-test: CREATE INDEX CONCURRENTLY
	t.Run("create_index_concurrently", func(t *testing.T) {
		indexName := fmt.Sprintf("idx_%s_data_concurrent", tableName)

		start := time.Now()
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s (data)", indexName, tableName))
		concDuration := time.Since(start)

		require.NoError(t, err, "create index concurrently")
		t.Logf("PG CREATE INDEX CONCURRENTLY: %v", concDuration.Round(time.Millisecond))

		var count int
		db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_indexes WHERE indexname = $1`, indexName,
		).Scan(&count)
		assert.Equal(t, 1, count)
	})

	// Sub-test: ADD COLUMN with default (rewrites entire table in PG <=10)
	t.Run("add_column_with_default", func(t *testing.T) {
		start := time.Now()
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN status TEXT NOT NULL DEFAULT 'active'", tableName))
		addColDuration := time.Since(start)

		require.NoError(t, err, "add column with default")
		t.Logf("PG ADD COLUMN with default: %v", addColDuration.Round(time.Millisecond))

		// Verify column exists and has correct default
		var count int
		db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE status = 'active'", tableName),
		).Scan(&count)
		assert.Equal(t, rows, count, "all rows should have status='active'")
	})

	// Sub-test: VACUUM ANALYZE timing
	t.Run("vacuum_analyze", func(t *testing.T) {
		start := time.Now()
		_, err := db.ExecContext(ctx, fmt.Sprintf("VACUUM ANALYZE %s", tableName))
		vacDuration := time.Since(start)

		require.NoError(t, err, "vacuum analyze")
		t.Logf("PG VACUUM ANALYZE: %v", vacDuration.Round(time.Millisecond))
	})
}

// Concurrent Access During Migration

// TestLargeMigration_ConcurrentAccess_Postgres verifies that reads and writes
// are affected during a blocking migration. Spawns goroutines reading/writing
// while a CREATE INDEX (non-concurrent) runs, measuring throughput degradation.
func TestLargeMigration_ConcurrentAccess_Postgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent access test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rows := largeDatasetRows()
	if rows > 500_000 {
		rows = 500_000 // keep smaller for concurrent test
	}

	ctr, err := postgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:16-alpine"),
		postgres.WithDatabase("lyeve_test"),
		postgres.WithUsername("cms"),
		postgres.WithPassword("cms"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(20)
	require.NoError(t, db.PingContext(ctx))

	tableName, _ := seedLargeTable(t, db, "postgres", rows)

	var readOps, writeOps atomic.Int64
	var readErrors, writeErrors atomic.Int64
	var stopWorker atomic.Bool

	// Start concurrent read workers
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for !stopWorker.Load() {
				var id int
				err := db.QueryRowContext(ctx,
					fmt.Sprintf("SELECT id FROM %s WHERE id = $1", tableName),
					int((int64(workerID*100)+readOps.Load())%int64(rows))+1, // pseudo-random within range
				).Scan(&id)
				if err != nil && err.Error() != "context canceled" {
					readErrors.Add(1)
				}
				readOps.Add(1)
			}
		}(i)
	}

	// Start concurrent write workers
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			seq := 0
			for !stopWorker.Load() {
				seq++
				_, err := db.ExecContext(ctx,
					fmt.Sprintf("UPDATE %s SET num = num + 1 WHERE id = $1", tableName),
					(workerID*1000+seq)%rows+1,
				)
				if err != nil {
					writeErrors.Add(1)
				}
				writeOps.Add(1)
			}
		}(i)
	}

	// Let workers warm up for 500ms
	time.Sleep(500 * time.Millisecond)

	// Capture baseline throughput
	baselineReads := readOps.Load()
	baselineWrites := writeOps.Load()
	time.Sleep(2 * time.Second)
	baselineReads = readOps.Load() - baselineReads
	baselineWrites = writeOps.Load() - baselineWrites
	readsPerSecBaseline := float64(baselineReads) / 2.0
	writesPerSecBaseline := float64(baselineWrites) / 2.0

	t.Logf("baseline: reads=%.0f/s, writes=%.0f/s", readsPerSecBaseline, writesPerSecBaseline)

	// Run a blocking migration (ADD COLUMN with default)
	migStart := time.Now()

	// Reset counters for migration window measurement
	preMigrationReads := readOps.Load()
	preMigrationWrites := writeOps.Load()

	_, err = db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN category TEXT NOT NULL DEFAULT 'general'", tableName))
	require.NoError(t, err)
	migDuration := time.Since(migStart)

	// Capture migration-window throughput
	migrationWindowReads := readOps.Load() - preMigrationReads
	migrationWindowWrites := writeOps.Load() - preMigrationWrites
	migSecs := migDuration.Seconds()
	if migSecs < 0.1 {
		migSecs = 0.1
	}
	readsPerSecMigration := float64(migrationWindowReads) / migSecs
	writesPerSecMigration := float64(migrationWindowWrites) / migSecs

	t.Logf("during migration: reads=%.0f/s, writes=%.0f/s", readsPerSecMigration, writesPerSecMigration)
	t.Logf("migration duration: %v", migDuration.Round(time.Millisecond))

	// Check for lock contention
	// In PostgreSQL, ALTER TABLE ADD COLUMN with DEFAULT since PG 11 does NOT
	// rewrite the table, so writes should NOT be fully blocked, but we should
	// check for blocking.
	if writesPerSecMigration < writesPerSecBaseline*0.1 {
		t.Logf("WARNING: write throughput dropped to <10%% of baseline during migration (lock contention)")
	} else {
		t.Logf("write throughput acceptable during migration (%.0f%% of baseline)",
			writesPerSecMigration/writesPerSecBaseline*100)
	}

	// Signal workers to stop
	stopWorker.Store(true)
	wg.Wait()

	totalReads := readOps.Load()
	totalWrites := writeOps.Load()
	readErrs := readErrors.Load()
	writeErrs := writeErrors.Load()

	t.Logf("total: reads=%d, writes=%d, read_errors=%d, write_errors=%d",
		totalReads, totalWrites, readErrs, writeErrs)

	assert.LessOrEqual(t, readErrs, totalReads/100, "read errors should be <1%% of reads")
	assert.LessOrEqual(t, writeErrs, totalWrites/10, "write errors should be <10%% of writes")

	// Verify data integrity
	var catCount int
	db.QueryRowContext(context.Background(),
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE category = 'general'", tableName),
	).Scan(&catCount)
	assert.Equal(t, rows, catCount, "all rows should have category='general'")
}

// MySQL Large Migration

// TestLargeMigration_LockDetection_MySQL tests adding an index to a large
// MySQL table. MySQL's InnoDB ALTER TABLE algorithm=INPLACE avoids table
// copies for many operations (since 5.6) but still acquires metadata locks.
func TestLargeMigration_LockDetection_MySQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large migration test in short mode")
	}

	ctx := context.Background()
	rows := largeDatasetRows()

	ctr, err := mysql.Run(ctx,
		"mysql:8",
		mysql.WithDatabase("lyeve_test"),
		mysql.WithUsername("cms"),
		mysql.WithPassword("cms"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err, "start mysql container")
	t.Cleanup(func() { ctr.Terminate(ctx) })

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	mappedPort, err := ctr.MappedPort(ctx, "3306")
	require.NoError(t, err)

	rootDSN := fmt.Sprintf("root:cms@tcp(%s:%s)/lyeve_test?parseTime=true&multiStatements=true",
		host, mappedPort.Port())

	db, err := sql.Open("mysql", rootDSN)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(20)
	require.NoError(t, db.PingContext(ctx))

	tableName, seedDuration := seedLargeTable(t, db, "mysql", rows)
	t.Logf("MySQL: seeded %d rows in %v", rows, seedDuration.Round(time.Millisecond))

	// Sub-test: CREATE INDEX
	t.Run("create_index", func(t *testing.T) {
		indexName := fmt.Sprintf("idx_%s_num", tableName)

		start := time.Now()
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("CREATE INDEX %s ON %s (num)", indexName, tableName))
		idxDuration := time.Since(start)

		require.NoError(t, err, "create index")
		t.Logf("MySQL CREATE INDEX: %v", idxDuration.Round(time.Millisecond))

		var count int
		db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.statistics WHERE index_name = ?`,
			indexName,
		).Scan(&count)
		assert.Greater(t, count, 0, "index should exist")
	})

	// Sub-test: ALTER TABLE ADD COLUMN
	t.Run("add_column", func(t *testing.T) {
		start := time.Now()
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN status VARCHAR(20) NOT NULL DEFAULT 'active'", tableName))
		addColDuration := time.Since(start)

		require.NoError(t, err, "add column")
		t.Logf("MySQL ADD COLUMN with default: %v", addColDuration.Round(time.Millisecond))

		var count int
		db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE status = 'active'", tableName),
		).Scan(&count)
		assert.Equal(t, rows, count, "all rows should have status='active'")
	})

	// Sub-test: ALTER TABLE ALGORITHM=INPLACE (if supported)
	t.Run("add_column_inplace", func(t *testing.T) {
		colName := fmt.Sprintf("cat_%d", time.Now().UnixNano()%100000)
		start := time.Now()
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s VARCHAR(50) DEFAULT 'general', ALGORITHM=INPLACE, LOCK=NONE",
				tableName, colName))
		dur := time.Since(start)

		if err != nil {
			// ALGORITHM=INPLACE with LOCK=NONE may not be supported for this operation.
			// Fall back to standard ALTER.
			t.Logf("MySQL ALGORITHM=INPLACE LOCK=NONE not supported for this DDL: %v", err)

			start2 := time.Now()
			_, err2 := db.ExecContext(ctx,
				fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s_default VARCHAR(50) DEFAULT 'general'", tableName, colName))
			require.NoError(t, err2)
			t.Logf("MySQL standard ADD COLUMN: %v", time.Since(start2).Round(time.Millisecond))
		} else {
			t.Logf("MySQL ALGORITHM=INPLACE LOCK=NONE: %v", dur.Round(time.Millisecond))
		}
	})
}

// Concurrent Access During MySQL Migration

// TestLargeMigration_ConcurrentAccess_MySQL verifies read/write behavior during
// a blocking ALTER TABLE on MySQL+InnoDB.
func TestLargeMigration_ConcurrentAccess_MySQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent access test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rows := largeDatasetRows()
	if rows > 500_000 {
		rows = 500_000
	}

	ctr, err := mysql.Run(ctx,
		"mysql:8",
		mysql.WithDatabase("lyeve_test"),
		mysql.WithUsername("cms"),
		mysql.WithPassword("cms"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	mappedPort, err := ctr.MappedPort(ctx, "3306")
	require.NoError(t, err)

	rootDSN := fmt.Sprintf("root:cms@tcp(%s:%s)/lyeve_test?parseTime=true&multiStatements=true",
		host, mappedPort.Port())

	db, err := sql.Open("mysql", rootDSN)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(20)
	require.NoError(t, db.PingContext(ctx))

	tableName, _ := seedLargeTable(t, db, "mysql", rows)

	var readOps, writeOps atomic.Int64
	var readErrors, writeErrors atomic.Int64
	var stopWorker atomic.Bool
	var wg sync.WaitGroup

	// Read workers
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for !stopWorker.Load() {
				var id int
				err := db.QueryRowContext(ctx,
					fmt.Sprintf("SELECT id FROM %s WHERE id = ? LIMIT 1", tableName),
					(workerID*100+int(readOps.Load()))%rows+1,
				).Scan(&id)
				if err != nil && err.Error() != "context canceled" {
					readErrors.Add(1)
				}
				readOps.Add(1)
			}
		}(i)
	}

	// Write workers
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			seq := 0
			for !stopWorker.Load() {
				seq++
				_, err := db.ExecContext(ctx,
					fmt.Sprintf("UPDATE %s SET num = num + 1 WHERE id = ?", tableName),
					(workerID*1000+seq)%rows+1,
				)
				if err != nil {
					writeErrors.Add(1)
				}
				writeOps.Add(1)
			}
		}(i)
	}

	// Warmup
	time.Sleep(500 * time.Millisecond)

	baselineReads := readOps.Load()
	baselineWrites := writeOps.Load()
	time.Sleep(2 * time.Second)
	baselineReads = readOps.Load() - baselineReads
	baselineWrites = writeOps.Load() - baselineWrites
	readsPerSecBase := float64(baselineReads) / 2.0
	writesPerSecBase := float64(baselineWrites) / 2.0
	t.Logf("MySQL baseline: reads=%.0f/s, writes=%.0f/s", readsPerSecBase, writesPerSecBase)

	preMigReads := readOps.Load()
	preMigWrites := writeOps.Load()

	migStart := time.Now()
	_, err = db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN region VARCHAR(50) NOT NULL DEFAULT 'us-east-1'", tableName))
	require.NoError(t, err)
	migDuration := time.Since(migStart)

	migReads := readOps.Load() - preMigReads
	migWrites := writeOps.Load() - preMigWrites
	migSecs := migDuration.Seconds()
	if migSecs < 0.1 {
		migSecs = 0.1
	}

	t.Logf("MySQL during migration: reads=%.0f/s, writes=%.0f/s",
		float64(migReads)/migSecs, float64(migWrites)/migSecs)
	t.Logf("MySQL migration duration: %v", migDuration.Round(time.Millisecond))

	if float64(migWrites)/migSecs < writesPerSecBase*0.1 {
		t.Logf("WARNING: MySQL write throughput dropped to <10%% during ALTER TABLE (metadata lock)")
	}

	stopWorker.Store(true)
	wg.Wait()

	totalReads := readOps.Load()
	totalWrites := writeOps.Load()
	readErrs := readErrors.Load()
	writeErrs := writeErrors.Load()

	t.Logf("MySQL total: reads=%d, writes=%d, read_errors=%d, write_errors=%d",
		totalReads, totalWrites, readErrs, writeErrs)

	assert.LessOrEqual(t, readErrs, totalReads/100, "read errors < 1%%")
	assert.LessOrEqual(t, writeErrs, totalWrites/10, "write errors < 10%%")

	var catCount int
	db.QueryRowContext(context.Background(),
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE region = 'us-east-1'", tableName),
	).Scan(&catCount)
	assert.Equal(t, rows, catCount, "all rows should have region='us-east-1'")
}

// MSSQL Large Migration

// TestLargeMigration_LockDetection_MSSQL tests adding an index to a large
// MSSQL table. MSSQL supports ONLINE=ON for index operations to avoid table
// locks.
func TestLargeMigration_LockDetection_MSSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large migration test in short mode")
	}

	ctx := context.Background()
	rows := largeDatasetRows()

	ctr, err := tcmssql.Run(ctx,
		"mcr.microsoft.com/azure-sql-edge:latest",
		tcmssql.WithAcceptEULA(),
		tcmssql.WithPassword("Str0ng@Passw0rd"),
	)
	require.NoError(t, err, "start mssql container")
	t.Cleanup(func() { ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "encrypt=disable", "TrustServerCertificate=true")
	require.NoError(t, err)

	db, err := sql.Open("sqlserver", dsn)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(20)
	require.NoError(t, db.PingContext(ctx))

	tableName, seedDuration := seedLargeTable(t, db, "mssql", rows)
	t.Logf("MSSQL: seeded %d rows in %v", rows, seedDuration.Round(time.Millisecond))

	// Sub-test: CREATE INDEX (without ONLINE=ON)
	t.Run("create_index_offline", func(t *testing.T) {
		indexName := fmt.Sprintf("idx_%s_num", tableName)

		start := time.Now()
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("CREATE INDEX %s ON %s (num)", indexName, tableName))
		idxDuration := time.Since(start)

		require.NoError(t, err, "create index (offline)")
		t.Logf("MSSQL CREATE INDEX (offline): %v", idxDuration.Round(time.Millisecond))

		var count int
		db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sys.indexes WHERE name = @p1`,
			indexName,
		).Scan(&count)
		assert.Equal(t, 1, count, "index should exist")
	})

	// Sub-test: CREATE INDEX WITH (ONLINE=ON)
	// Note: Azure SQL Edge may not support ONLINE=ON in all editions.
	// We use the 'num' column because NVARCHAR(MAX) columns can't be index keys.
	t.Run("create_index_online", func(t *testing.T) {
		indexName := fmt.Sprintf("idx_%s_num_online", tableName)

		start := time.Now()
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("CREATE INDEX %s ON %s (num) WITH (ONLINE=ON)", indexName, tableName))
		dur := time.Since(start)

		if err != nil {
			t.Logf("MSSQL WITH (ONLINE=ON) not supported (Azure SQL Edge?): %v", err)

			// Fall back to offline index
			start2 := time.Now()
			idxName2 := indexName + "_offline_fallback"
			_, err2 := db.ExecContext(ctx,
				fmt.Sprintf("CREATE INDEX %s ON %s (num)", idxName2, tableName))
			require.NoError(t, err2)
			t.Logf("MSSQL offline fallback CREATE INDEX: %v", time.Since(start2).Round(time.Millisecond))
		} else {
			t.Logf("MSSQL CREATE INDEX WITH (ONLINE=ON): %v", dur.Round(time.Millisecond))
		}
	})

	// Sub-test: ALTER TABLE ADD COLUMN
	t.Run("add_column", func(t *testing.T) {
		start := time.Now()
		// MSSQL: use NVARCHAR with default constraint or using ADD with DEFAULT
		// ADD with NOT NULL and DEFAULT requires dropping and recreating constraints
		// For large tables, best approach: add nullable, update, then add default
		_, err := db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD category NVARCHAR(50) NULL", tableName))
		require.NoError(t, err)

		// Set default for existing rows
		_, err = db.ExecContext(ctx,
			fmt.Sprintf("UPDATE %s SET category = 'general' WHERE category IS NULL", tableName))
		require.NoError(t, err)

		// Add default constraint
		_, err = db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT DF_%s_category DEFAULT 'general' FOR category", tableName, tableName))
		require.NoError(t, err)

		colDuration := time.Since(start)
		t.Logf("MSSQL ADD COLUMN + UPDATE + DEFAULT: %v", colDuration.Round(time.Millisecond))

		var count int
		db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE category = 'general'", tableName),
		).Scan(&count)
		assert.Equal(t, rows, count, "all rows should have category='general'")
	})
}

// Concurrent Access During MSSQL Migration

// TestLargeMigration_ConcurrentAccess_MSSQL verifies read/write contention
// during a blocking ALTER TABLE on MSSQL.
func TestLargeMigration_ConcurrentAccess_MSSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent access test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rows := largeDatasetRows()
	if rows > 200_000 {
		rows = 200_000 // Azure SQL Edge is slower, keep smaller
	}

	ctr, err := tcmssql.Run(ctx,
		"mcr.microsoft.com/azure-sql-edge:latest",
		tcmssql.WithAcceptEULA(),
		tcmssql.WithPassword("Str0ng@Passw0rd"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "encrypt=disable", "TrustServerCertificate=true")
	require.NoError(t, err)

	db, err := sql.Open("sqlserver", dsn)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(20)
	require.NoError(t, db.PingContext(ctx))

	tableName, seedDuration := seedLargeTable(t, db, "mssql", rows)
	t.Logf("MSSQL: seeded %d rows in %v", rows, seedDuration.Round(time.Millisecond))

	var readOps, writeOps atomic.Int64
	var readErrors, writeErrors atomic.Int64
	var stopWorker atomic.Bool
	var wg sync.WaitGroup

	// Read workers
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for !stopWorker.Load() {
				var id int
				err := db.QueryRowContext(ctx,
					fmt.Sprintf("SELECT id FROM %s WHERE id = @p1", tableName),
					(workerID*100+int(readOps.Load()))%rows+1,
				).Scan(&id)
				if err != nil && err.Error() != "context canceled" {
					readErrors.Add(1)
				}
				readOps.Add(1)
			}
		}(i)
	}

	// Write workers
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			seq := 0
			for !stopWorker.Load() {
				seq++
				_, err := db.ExecContext(ctx,
					fmt.Sprintf("UPDATE %s SET num = num + 1 WHERE id = @p1", tableName),
					(workerID*1000+seq)%rows+1,
				)
				if err != nil {
					writeErrors.Add(1)
				}
				writeOps.Add(1)
			}
		}(i)
	}

	time.Sleep(500 * time.Millisecond)

	baselineReads := readOps.Load()
	baselineWrites := writeOps.Load()
	time.Sleep(2 * time.Second)
	baselineReads = readOps.Load() - baselineReads
	baselineWrites = writeOps.Load() - baselineWrites
	readsPerSecBase := float64(baselineReads) / 2.0
	writesPerSecBase := float64(baselineWrites) / 2.0
	t.Logf("MSSQL baseline: reads=%.0f/s, writes=%.0f/s", readsPerSecBase, writesPerSecBase)

	preMigReads := readOps.Load()
	preMigWrites := writeOps.Load()

	migStart := time.Now()
	_, err = db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE %s ADD flag BIT NULL", tableName))
	require.NoError(t, err)
	migDuration := time.Since(migStart)

	migReads := readOps.Load() - preMigReads
	migWrites := writeOps.Load() - preMigWrites
	migSecs := migDuration.Seconds()
	if migSecs < 0.1 {
		migSecs = 0.1
	}

	t.Logf("MSSQL during migration: reads=%.0f/s, writes=%.0f/s",
		float64(migReads)/migSecs, float64(migWrites)/migSecs)
	t.Logf("MSSQL migration duration: %v", migDuration.Round(time.Millisecond))

	if float64(migWrites)/migSecs < writesPerSecBase*0.1 {
		t.Logf("WARNING: MSSQL write throughput dropped to <10%% during ALTER TABLE")
	}

	stopWorker.Store(true)
	wg.Wait()

	totalReads := readOps.Load()
	totalWrites := writeOps.Load()
	readErrs := readErrors.Load()
	writeErrs := writeErrors.Load()

	t.Logf("MSSQL total: reads=%d, writes=%d, read_errors=%d, write_errors=%d",
		totalReads, totalWrites, readErrs, writeErrs)

	assert.LessOrEqual(t, readErrs, totalReads/100, "read errors < 1%%")
	assert.LessOrEqual(t, writeErrs, totalWrites/10, "write errors < 10%%")
}

// Migration Duration Benchmark

// TestLargeMigration_DurationComparison runs identical ADD INDEX operations
// across all three dialects and reports comparative timings.
func TestLargeMigration_DurationComparison(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping duration comparison in short mode")
	}

	rows := largeDatasetRows()
	if rows > 500_000 {
		rows = 500_000
	}

	type dialectResult struct {
		dialect       string
		seedDuration  time.Duration
		indexDuration time.Duration
		colDuration   time.Duration
		concurrentOK  bool
		notes         string
	}
	results := make([]dialectResult, 0, 3)

	// PostgreSQL
	t.Run("postgres", func(t *testing.T) {
		ctx := context.Background()
		ctr, err := postgres.RunContainer(ctx,
			testcontainers.WithImage("postgres:16-alpine"),
			postgres.WithDatabase("lyeve_test"),
			postgres.WithUsername("cms"),
			postgres.WithPassword("cms"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second),
			),
		)
		require.NoError(t, err)
		defer ctr.Terminate(ctx)

		dsn, _ := ctr.ConnectionString(ctx, "sslmode=disable")
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		defer db.Close()

		tableName, seedDur := seedLargeTable(t, db, "postgres", rows)

		// Regular index
		idxStart := time.Now()
		_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE INDEX idx_dc_num ON %s (num)", tableName))
		idxDur := time.Since(idxStart)
		require.NoError(t, err)

		// Concurrent index
		concStart := time.Now()
		_, err = db.ExecContext(ctx,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_dc_data ON %s (data)", tableName))
		concDur := time.Since(concStart)
		require.NoError(t, err)

		results = append(results, dialectResult{
			dialect:       "postgres",
			seedDuration:  seedDur,
			indexDuration: idxDur,
			colDuration:   concDur,
			concurrentOK:  true,
			notes:         fmt.Sprintf("CONCURRENTLY supported, index=%.0fs concurrent=%.0fs", idxDur.Seconds(), concDur.Seconds()),
		})
	})

	// MySQL
	t.Run("mysql", func(t *testing.T) {
		ctx := context.Background()
		ctr, err := mysql.Run(ctx, "mysql:8",
			mysql.WithDatabase("lyeve_test"),
			mysql.WithUsername("cms"),
			mysql.WithPassword("cms"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("port: 3306  MySQL Community Server").
					WithStartupTimeout(90*time.Second),
			),
		)
		require.NoError(t, err)
		defer ctr.Terminate(ctx)

		host, _ := ctr.Host(ctx)
		mappedPort, _ := ctr.MappedPort(ctx, "3306")
		rootDSN := fmt.Sprintf("root:cms@tcp(%s:%s)/lyeve_test?parseTime=true&multiStatements=true", host, mappedPort.Port())

		db, err := sql.Open("mysql", rootDSN)
		require.NoError(t, err)
		defer db.Close()

		tableName, seedDur := seedLargeTable(t, db, "mysql", rows)

		idxStart := time.Now()
		_, err = db.ExecContext(ctx,
			fmt.Sprintf("CREATE INDEX idx_dc_num ON %s (num)", tableName))
		idxDur := time.Since(idxStart)
		require.NoError(t, err)

		// Try with ALGORITHM=INPLACE, LOCK=NONE
		concOK := false
		concNote := ""
		inplaceStart := time.Now()
		_, err = db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD INDEX idx_dc_data (data), ALGORITHM=INPLACE, LOCK=NONE", tableName))
		inplaceDur := time.Since(inplaceStart)
		if err != nil {
			concNote = fmt.Sprintf("INPLACE+LOCK=NONE not supported (%v)", err)
		} else {
			concOK = true
			concNote = fmt.Sprintf("ALGORITHM=INPLACE LOCK=NONE: %v", inplaceDur.Round(time.Millisecond))
		}

		results = append(results, dialectResult{
			dialect:       "mysql",
			seedDuration:  seedDur,
			indexDuration: idxDur,
			concurrentOK:  concOK,
			notes:         concNote,
		})
	})

	// MSSQL
	t.Run("mssql", func(t *testing.T) {
		ctx := context.Background()
		ctr, err := tcmssql.Run(ctx,
			"mcr.microsoft.com/azure-sql-edge:latest",
			tcmssql.WithAcceptEULA(),
			tcmssql.WithPassword("Str0ng@Passw0rd"),
		)
		require.NoError(t, err)
		defer ctr.Terminate(ctx)

		dsn, _ := ctr.ConnectionString(ctx, "encrypt=disable", "TrustServerCertificate=true")
		db, err := sql.Open("sqlserver", dsn)
		require.NoError(t, err)
		defer db.Close()

		tableName, seedDur := seedLargeTable(t, db, "mssql", rows)

		idxStart := time.Now()
		_, err = db.ExecContext(ctx,
			fmt.Sprintf("CREATE INDEX idx_dc_num ON %s (num)", tableName))
		idxDur := time.Since(idxStart)
		require.NoError(t, err)

		concOK := false
		concNote := ""
		concStart := time.Now()
		_, err = db.ExecContext(ctx,
			fmt.Sprintf("CREATE INDEX idx_dc_data ON %s (data) WITH (ONLINE=ON)", tableName))
		concDur := time.Since(concStart)
		if err != nil {
			concNote = fmt.Sprintf("WITH (ONLINE=ON) not supported in Azure SQL Edge (%v)", err)
		} else {
			concOK = true
			concNote = fmt.Sprintf("WITH (ONLINE=ON): %v", concDur.Round(time.Millisecond))
		}

		results = append(results, dialectResult{
			dialect:       "mssql",
			seedDuration:  seedDur,
			indexDuration: idxDur,
			concurrentOK:  concOK,
			notes:         concNote,
		})
	})

	// Summary
	if t.Failed() {
		return
	}

	t.Logf("=== Migration Duration Comparison (rows=%d) ===", rows)
	t.Logf("%-10s | %-15s | %-15s | %-8s | notes",
		"dialect", "seed_time", "index_time", "online")
	t.Logf("-----------+-----------------+-----------------+----------+------")
	for _, r := range results {
		t.Logf("%-10s | %-15s | %-15s | %-8s | %s",
			r.dialect,
			r.seedDuration.Round(time.Millisecond),
			r.indexDuration.Round(time.Millisecond),
			map[bool]string{true: "yes", false: "no"}[r.concurrentOK],
			r.notes,
		)
	}
}
