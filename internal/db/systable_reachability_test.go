//go:build !mutest

package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// TestSysTableReachability_FromTenantScopedConn covers the half of the tenancy
// contract that cross-tenant isolation does not: the sys_* catalog tables are
// created once in the engine's own database and separated by a tenant_id
// column, so a tenant-scoped connection has to be able to read them.
//
// PostgreSQL gets that for free because its Apply leaves public on the
// search_path. MySQL and SQL Server switch databases outright, so an
// unqualified sys_* reference would resolve inside the tenant's own database:
//
//	Error 1146 (42S02): Table 'tenant_x.sys_users' doesn't exist
//	Msg 208: Invalid object name 'sys_users'.
//
// Postgres passes with or without the rewrite, so this runs on all three.
func TestSysTableReachability_FromTenantScopedConn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dialects := []struct {
		name string
		pool func(*testing.T) db.DB
	}{
		{"postgres", func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{"mysql", func(t *testing.T) db.DB { return testdb.MySQL(t) }},
		{"mssql", func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
	}

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			pool := d.pool(t)
			ctx := context.Background()

			slug := fmt.Sprintf("sysr_%d", time.Now().UnixNano()%100000)
			createTenant(t, ctx, pool, slug)
			t.Cleanup(func() { dropTenant(t, pool, slug) })

			tctx, conn := acquireTenant(t, ctx, pool, slug)
			defer conn.Close()

			// Plain table position.
			var n int
			row, err := pool.QueryRow(tctx, "SELECT COUNT(*) FROM sys_users")
			if err != nil {
				t.Fatalf("query sys_users from tenant scope: %v", err)
			}
			if err := row.Scan(&n); err != nil {
				t.Fatalf("sys_users unreachable from tenant scope: %v", err)
			}

			// Table position plus a column reference through the correlation
			// name, which is the form a qualified table has to keep working.
			var m int
			row, err = pool.QueryRow(tctx,
				"SELECT COUNT(*) FROM sys_users WHERE sys_users.email IS NOT NULL")
			if err != nil {
				t.Fatalf("query qualified column from tenant scope: %v", err)
			}
			if err := row.Scan(&m); err != nil {
				t.Fatalf("sys_users column reference unreachable from tenant scope: %v", err)
			}

			// A write has to land in the engine's database too, and be visible
			// from a connection that never entered tenant scope.
			id := fmt.Sprintf("sysr-%d", time.Now().UnixNano())
			if _, err := pool.Exec(tctx,
				"INSERT INTO sys_users (id, email, password_hash, created_at, updated_at) "+
					"VALUES ($1, $2, $3, $4, $5)",
				newUUID(t, pool), id+"@lyeve.test", "x", time.Now().UTC(), time.Now().UTC(),
			); err != nil {
				t.Fatalf("insert into sys_users from tenant scope: %v", err)
			}
			var seen int
			row, err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM sys_users WHERE email = $1", id+"@lyeve.test")
			if err != nil {
				t.Fatalf("read back outside tenant scope: %v", err)
			}
			if err := row.Scan(&seen); err != nil {
				t.Fatalf("scan read back: %v", err)
			}
			if seen != 1 {
				t.Errorf("row written from tenant scope did not land in the engine database: found %d", seen)
			}

			// The isolation boundary itself is untouched: the tenant's own
			// tables still resolve inside the tenant.
			if _, err := pool.Exec(tctx, "INSERT INTO items (label) VALUES ($1)", "gamma"); err != nil {
				t.Fatalf("tenant-local insert: %v", err)
			}
			assertSees(t, pool, tctx, "gamma", true)
		})
	}
}

// newUUID returns an id in whatever textual form the dialect's sys_users
// primary key accepts.
func newUUID(t *testing.T, pool db.DB) string {
	t.Helper()
	var id string
	var q string
	switch pool.Engine() {
	case "mysql":
		q = "SELECT UUID()"
	case "mssql":
		q = "SELECT CONVERT(NVARCHAR(36), NEWID())"
	default:
		q = "SELECT gen_random_uuid()::text"
	}
	row, err := pool.QueryRow(context.Background(), q)
	if err != nil {
		t.Fatalf("new uuid: %v", err)
	}
	if err := row.Scan(&id); err != nil {
		t.Fatalf("scan uuid: %v", err)
	}
	return id
}
