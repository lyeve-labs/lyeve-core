//go:build !mutest

package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// TestTenantIsolation_CrossTenantReads verifies that schema-per-tenant
// (Postgres) and database-per-tenant (MySQL, MSSQL) all prevent cross-tenant
// reads. Edge cases covered: empty tenant, deleted tenant, and slugs with
// underscores.
func TestTenantIsolation_CrossTenantReads(t *testing.T) {
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

			// Slugs with underscores exercise the safeSlugRe boundary.
			slugA := fmt.Sprintf("tz_a_%d", time.Now().UnixNano()%10000)
			slugB := fmt.Sprintf("tz_b_%d", time.Now().UnixNano()%10000)

			for _, s := range []string{slugA, slugB} {
				createTenant(t, ctx, pool, s)
			}
			t.Cleanup(func() {
				dropTenant(t, pool, slugA)
				dropTenant(t, pool, slugB)
			})

			ctxA, connA := acquireTenant(t, ctx, pool, slugA)
			defer connA.Close()
			ctxB, connB := acquireTenant(t, ctx, pool, slugB)
			defer connB.Close()

			_, err := pool.Exec(ctxA, "INSERT INTO items (label) VALUES ($1)", "alpha")
			if err != nil {
				t.Fatalf("insert A: %v", err)
			}
			_, err = pool.Exec(ctxB, "INSERT INTO items (label) VALUES ($1)", "beta")
			if err != nil {
				t.Fatalf("insert B: %v", err)
			}

			// Cross-tenant reads: each tenant sees only its own data.
			assertSees(t, pool, ctxA, "alpha", true)
			assertSees(t, pool, ctxA, "beta", false)
			assertSees(t, pool, ctxB, "beta", true)
			assertSees(t, pool, ctxB, "alpha", false)

			// Empty tenant: a newly created tenant with no rows.
			slugEmpty := fmt.Sprintf("tz_e_%d", time.Now().UnixNano()%10000)
			createTenant(t, ctx, pool, slugEmpty)
			t.Cleanup(func() { dropTenant(t, pool, slugEmpty) })
			ctxE, connE := acquireTenant(t, ctx, pool, slugEmpty)
			defer connE.Close()
			assertSees(t, pool, ctxE, "alpha", false)

			// Deleted tenant: drop tenant B, verify A is unaffected.
			dropTenant(t, pool, slugB)
			assertSees(t, pool, ctxA, "alpha", true)
		})
	}
}

// helpers

func createTenant(t *testing.T, ctx context.Context, pool db.DB, slug string) {
	t.Helper()
	switch pool.Engine() {
	case "postgres":
		schema := "tenant_" + slug
		_, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema)
		if err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
		_, err = pool.Exec(ctx,
			"CREATE TABLE IF NOT EXISTS "+schema+".items (id SERIAL PRIMARY KEY, label TEXT)")
		if err != nil {
			t.Fatalf("create table in %s: %v", schema, err)
		}
	case "mysql":
		dbName := "tenant_" + slug
		_, err := pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+dbName+"`")
		if err != nil {
			t.Fatalf("create db %s: %v", dbName, err)
		}
		_, err = pool.Exec(ctx,
			"CREATE TABLE IF NOT EXISTS `"+dbName+"`.items (id INT AUTO_INCREMENT PRIMARY KEY, label VARCHAR(255))")
		if err != nil {
			t.Fatalf("create table in %s: %v", dbName, err)
		}
	case "mssql":
		dbName := "tenant_" + slug
		_, err := pool.Exec(ctx, "CREATE DATABASE ["+dbName+"]")
		if err != nil {
			t.Fatalf("create db %s: %v", dbName, err)
		}
		_, err = pool.Exec(ctx,
			"CREATE TABLE ["+dbName+"].dbo.items (id INT IDENTITY PRIMARY KEY, label NVARCHAR(255))")
		if err != nil {
			t.Fatalf("create table in %s: %v", dbName, err)
		}
	}
}

func dropTenant(t *testing.T, pool db.DB, slug string) {
	t.Helper()
	switch pool.Engine() {
	case "postgres":
		schema := "tenant_" + slug
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	case "mysql":
		dbName := "tenant_" + slug
		pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`")
	case "mssql":
		dbName := "tenant_" + slug
		pool.Exec(context.Background(), "USE [master]")
		pool.Exec(context.Background(), "DROP DATABASE IF EXISTS ["+dbName+"]")
	}
}

func acquireTenant(t *testing.T, ctx context.Context, pool db.DB, slug string) (context.Context, *sql.Conn) {
	t.Helper()
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	switch pool.Engine() {
	case "postgres":
		tc := db.NewPostgresSchemaTenancy(func(context.Context) string { return slug })
		if err := tc.Apply(ctx, conn); err != nil {
			conn.Close()
			t.Fatalf("pg apply: %v", err)
		}
	case "mysql":
		tc := db.NewMySQLDatabaseTenancy(func(context.Context) string { return slug }, "lyeve_test")
		if err := tc.Apply(ctx, conn); err != nil {
			conn.Close()
			t.Fatalf("mysql apply: %v", err)
		}
	case "mssql":
		tc := db.NewMSSQLDatabaseTenancy(func(context.Context) string { return slug }, "master")
		if err := tc.Apply(ctx, conn); err != nil {
			conn.Close()
			t.Fatalf("mssql apply: %v", err)
		}
	}
	return db.WithTenantConn(ctx, conn), conn
}

func assertSees(t *testing.T, pool db.DB, ctx context.Context, label string, want bool) {
	t.Helper()
	var got string
	row, qrErr := pool.QueryRow(ctx, "SELECT label FROM items WHERE label = $1", label)
	_ = qrErr
	err := row.Scan(&got)
	switch {
	case want && err != nil:
		t.Errorf("expected to find %q, got: %v", label, err)
	case !want && err == nil:
		t.Errorf("cross-tenant leak: unexpectedly found %q", label)
	}
}
