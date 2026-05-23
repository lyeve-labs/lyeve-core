//go:build !mutest

package db_test

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// QuerierRO: fallback to primary when no replica configured

func TestQuerierRO_FallbackToPrimary_WhenNoReplica(t *testing.T) {
	t.Parallel()

	pool := testdb.Postgres(t)
	ctx := context.Background()

	ro, err := pool.QuerierRO(ctx)
	if err != nil {
		t.Fatalf("QuerierRO should succeed when no replica: %v", err)
	}
	if ro == nil {
		t.Fatal("QuerierRO returned nil querier")
	}

	var got int
	row, qrErr := ro.QueryRow(ctx, "SELECT 1")
	_ = qrErr
	if err := row.Scan(&got); err != nil {
		t.Fatalf("read-only QueryRow failed: %v", err)
	}
	if got != 1 {
		t.Errorf("expected 1, got %d", got)
	}

	rows, err := ro.Query(ctx, "SELECT $1::int AS n", 42)
	if err != nil {
		t.Fatalf("read-only Query failed: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected at least one row")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

func TestQuerierRO_FallbackToPrimary_WhenNoReplica_MySQL(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("MySQL not enabled")
	}
	t.Parallel()

	pool := testdb.MySQL(t)
	ctx := context.Background()

	ro, err := pool.QuerierRO(ctx)
	if err != nil {
		t.Fatalf("QuerierRO should succeed when no replica (MySQL): %v", err)
	}
	if ro == nil {
		t.Fatal("QuerierRO returned nil querier")
	}

	var got int
	row, qrErr := ro.QueryRow(ctx, "SELECT 1")
	_ = qrErr
	if err := row.Scan(&got); err != nil {
		t.Fatalf("read-only QueryRow failed (MySQL): %v", err)
	}
	if got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
}

func TestQuerierRO_FallbackToPrimary_WhenNoReplica_MSSQL(t *testing.T) {
	if !testdb.ShouldTest("mssql") {
		t.Skip("MSSQL not enabled")
	}
	t.Parallel()

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	ro, err := pool.QuerierRO(ctx)
	if err != nil {
		t.Fatalf("QuerierRO should succeed when no replica (MSSQL): %v", err)
	}
	if ro == nil {
		t.Fatal("QuerierRO returned nil querier")
	}

	var got int
	row, qrErr := ro.QueryRow(ctx, "SELECT 1")
	_ = qrErr
	if err := row.Scan(&got); err != nil {
		t.Fatalf("read-only QueryRow failed (MSSQL): %v", err)
	}
	if got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
}

func TestQuerierRO_PingFails_ReturnsError(t *testing.T) {
	t.Parallel()

	pool := testdb.Postgres(t)
	ctx := context.Background()

	raw := pool.SQLDB()
	raw.Close()

	_, err := pool.QuerierRO(ctx)
	if err == nil {
		t.Fatal("expected ping error after close, got nil")
	}
}

// ConnectReplica

func TestConnectReplica_EmptyDSN_NoOp(t *testing.T) {
	t.Parallel()

	pool := testdb.Postgres(t)
	ctx := context.Background()

	if err := db.ConnectReplica(ctx, pool, "", 0); err != nil {
		t.Fatalf("ConnectReplica with empty DSN should be no-op: %v", err)
	}

	ro, err := pool.QuerierRO(ctx)
	if err != nil {
		t.Fatalf("QuerierRO after empty-replica ConnectReplica: %v", err)
	}
	if ro == nil {
		t.Fatal("expected non-nil querier")
	}
}
