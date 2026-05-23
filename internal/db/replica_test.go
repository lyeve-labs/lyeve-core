//go:build !mutest

package db

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Unit: QuerierRO routes to an explicitly-set replica
//
// Tests that when sqlDB.replica is set, QuerierRO returns a querier backed by
// the replica rather than the primary.

func TestQuerierRO_RoutesToReplicaWhenConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Start primary PostgreSQL container
	primaryCtr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("lyeve_primary"),
		postgres.WithUsername("cms"),
		postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start primary container: %v", err)
	}
	t.Cleanup(func() { primaryCtr.Terminate(context.Background()) }) //nolint:errcheck

	// Start replica PostgreSQL container
	replicaCtr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("lyeve_replica"),
		postgres.WithUsername("cms"),
		postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start replica container: %v", err)
	}
	t.Cleanup(func() { replicaCtr.Terminate(context.Background()) }) //nolint:errcheck

	primaryDSN, err := primaryCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("primary DSN: %v", err)
	}
	replicaDSN, err := replicaCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("replica DSN: %v", err)
	}

	// Connect to both
	primaryPool, err := Connect(ctx, primaryDSN, 4)
	if err != nil {
		t.Fatalf("connect primary: %v", err)
	}
	defer primaryPool.Close()

	replicaPool, err := Connect(ctx, replicaDSN, 4)
	if err != nil {
		t.Fatalf("connect replica: %v", err)
	}
	defer replicaPool.Close()

	// Migrate both, then give each its own table.
	if _, err := Migrate(primaryDSN, "../../migrations", nil); err != nil {
		t.Fatalf("migrate primary: %v", err)
	}
	if _, err := Migrate(replicaDSN, "../../migrations", nil); err != nil {
		t.Fatalf("migrate replica: %v", err)
	}

	// Insert distinct data on each pool
	const ddl = `CREATE TABLE IF NOT EXISTS replica_routing_probe (name TEXT PRIMARY KEY)`
	for _, p := range []DB{primaryPool, replicaPool} {
		if _, err := p.Exec(ctx, ddl); err != nil {
			t.Fatalf("create probe table: %v", err)
		}
	}
	if _, err := primaryPool.Exec(ctx, `INSERT INTO replica_routing_probe (name) VALUES ('primary_only') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert on primary: %v", err)
	}
	if _, err := replicaPool.Exec(ctx, `INSERT INTO replica_routing_probe (name) VALUES ('replica_only') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("insert on replica: %v", err)
	}

	// Wire replica into primary's sqlDB
	s, ok := primaryPool.(*sqlDB)
	if !ok {
		t.Fatal("primary pool is not *sqlDB")
	}
	s.replica = replicaPool.SQLDB()

	// QuerierRO should route to replica
	ro, err := primaryPool.QuerierRO(ctx)
	if err != nil {
		t.Fatalf("QuerierRO: %v", err)
	}

	// Replica-only row should be visible.
	var name string
	row, qrErr := ro.QueryRow(ctx, "SELECT name FROM replica_routing_probe WHERE name = 'replica_only'")
	err = row.Scan(&name)
	_ = qrErr
	if err != nil {
		t.Fatalf("read 'replica_only' via replica querier: %v", err)
	}
	if name != "replica_only" {
		t.Errorf("expected 'replica_only', got %q", name)
	}

	// Primary-only row should NOT be visible via replica querier.
	row, qrErr = ro.QueryRow(ctx, "SELECT name FROM replica_routing_probe WHERE name = 'primary_only'")
	err = row.Scan(&name)
	_ = qrErr
	if err == nil {
		t.Errorf("expected error for primary-only row via replica, got name=%q", name)
	}

	// Clean up: remove replica reference before teardown.
	s.replica = nil
}
