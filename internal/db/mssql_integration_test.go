//go:build !mutest

package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	tcmssql "github.com/testcontainers/testcontainers-go/modules/mssql"
)

// TestMSSQL_ConnectAndMigrate verifies the full MSSQL lifecycle:
//  1. Start an Azure SQL Edge container via testcontainers
//  2. Connect using the db.Connect seam (validates EngineFromDSN + driverNameFor)
//  3. Apply core migrations via db.Migrate
//  4. Verify a table from the init migration exists
//
// The test uses mcr.microsoft.com/azure-sql-edge (not the full SQL Server
// image) because it's smaller, starts faster, and covers the same T-SQL
// surface area the CMS targets.
func TestMSSQL_ConnectAndMigrate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()

	// Start Azure SQL Edge container.
	container, err := tcmssql.Run(ctx,
		"mcr.microsoft.com/azure-sql-edge:latest",
		tcmssql.WithAcceptEULA(),
		tcmssql.WithPassword("Strong@Passw0rd"),
		// Override the default wait strategy: Azure SQL Edge emits
		// "Started" instead of "Recovery is complete".
		// The listening-port check + timeout is sufficient.
	)
	require.NoError(t, err, "start Azure SQL Edge container")

	// Cleanup after test.
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	// Get the connection string (sqlserver:// format).
	dsn, err := container.ConnectionString(ctx, "encrypt=disable", "TrustServerCertificate=true")
	require.NoError(t, err, "build connection string")

	// Connect using the engine's Connect seam: this exercises
	// EngineFromDSN, driverNameFor, placeholder rewriting, and the
	// go-mssqldb driver all in one call.
	pool, err := Connect(ctx, dsn, 4)
	require.NoError(t, err, "connect to MSSQL via db.Connect")
	t.Cleanup(func() { pool.Close() })

	// Verify engine detection.
	require.Equal(t, "mssql", pool.Engine(), "engine should be mssql")

	// Ping to confirm connectivity.
	require.NoError(t, pool.Ping(ctx), "ping MSSQL")

	// Run the core migrations. db.Migrate resolves mssql -> mssql/ subdir.
	n, err := Migrate(dsn, "../../migrations", nil)
	require.NoError(t, err, "apply MSSQL migrations")
	t.Logf("applied %d MSSQL migrations", n)

	// Verify a migration table exists. sys_users is created by 001_init.
	var count int
	row, qrErr := pool.QueryRow(ctx, "SELECT COUNT(1) FROM sys_users")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	require.NoError(t, row.Scan(&count), "query sys_users")

	t.Logf("MSSQL integration: connected, migrated %d scripts, sys_users row count = %d", n, count)
}

// TestMSSQL_DialectRewrite verifies that the placeholder rewrite pipeline
// works correctly against a real MSSQL connection. $N placeholders should
// be rewritten to @pN by the sqlDB layer.
func TestMSSQL_DialectRewrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()

	container, err := tcmssql.Run(ctx,
		"mcr.microsoft.com/azure-sql-edge:latest",
		tcmssql.WithAcceptEULA(),
		tcmssql.WithPassword("Strong@Passw0rd"),
	)
	require.NoError(t, err, "start Azure SQL Edge container")
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "encrypt=disable", "TrustServerCertificate=true")
	require.NoError(t, err, "build connection string")

	pool, err := Connect(ctx, dsn, 4)
	require.NoError(t, err, "connect to MSSQL")
	t.Cleanup(func() { pool.Close() })

	// Exercise the placeholder rewrite via a parameterized query.
	// The sqlDB.rewrite layer converts $1 -> @p1 for MSSQL.
	row, qrErr := pool.QueryRow(ctx, "SELECT $1", 42)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	var result int
	require.NoError(t, row.Scan(&result), "scan parameterized query result")
	require.Equal(t, 42, result, "SELECT $1 should return 42 via @p1 rewrite")

	// Multi-parameter query.
	row, qrErr = pool.QueryRow(ctx, "SELECT $1 + $2", 100, 23)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	var sum int
	require.NoError(t, row.Scan(&sum), "scan multi-parameter query result")
	require.Equal(t, 123, sum, "100+23 should be 123")
}
