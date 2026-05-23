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

// A tenant teardown must not follow the pooled connection into the next
// request.
//
// MySQL does not close a session when the database it is sitting in is
// dropped. A same-session DROP leaves DATABASE() NULL and every unqualified
// reference failing with 1046 "No database selected". A DROP from elsewhere
// leaves the dead name in place and references fail with 1049. Either way the
// connection goes back to the pool and database/sql hands it to whoever asks
// next, so deleting one tenant would take out requests for every other tenant
// on that connection until it aged out of the pool.
//
// The single-tenant slug is the one that has to be tested, because it names no
// database of its own: an Apply that returned without touching the connection
// would inherit whatever the previous request left behind.
func TestMySQLTenancy_ADroppedTenantDatabaseDoesNotStrandTheNextRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	// One connection in the pool, so the connection the tenant request used is
	// necessarily the one the next request gets. The engine's pool is wider,
	// which spreads the damage over more requests rather than avoiding it.
	pool.SQLDB().SetMaxOpenConns(1)
	pool.SQLDB().SetMaxIdleConns(1)

	engineDB := db.DatabaseNameOf(pool)
	if engineDB == "" {
		t.Fatal("pool did not report its own database name")
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE tenancy_probe (owner VARCHAR(64))"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO tenancy_probe VALUES ('engine')"); err != nil {
		t.Fatalf("seed probe table: %v", err)
	}

	for _, tc := range []struct {
		name     string
		sameConn bool // drop through the tenant's own connection, or another
	}{
		{name: "dropped from the tenant request's own connection", sameConn: true},
		{name: "dropped from another connection", sameConn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slug := fmt.Sprintf("strand_%d", time.Now().UnixNano()%100000)
			tenantDB := "tenant_" + slug
			if _, err := pool.Exec(ctx, "CREATE DATABASE `"+tenantDB+"`"); err != nil {
				t.Fatalf("create tenant database: %v", err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+tenantDB+"`")
			})

			tenantReq := db.NewMySQLDatabaseTenancy(func(context.Context) string { return slug }, engineDB)
			defaultReq := db.NewMySQLDatabaseTenancy(func(context.Context) string { return "default" }, engineDB)

			// The tenant's request.
			conn, err := pool.Conn(ctx)
			if err != nil {
				t.Fatalf("acquire conn: %v", err)
			}
			if err := tenantReq.Apply(ctx, conn); err != nil {
				t.Fatalf("apply tenant isolation: %v", err)
			}

			if tc.sameConn {
				if _, err := conn.ExecContext(ctx, "DROP DATABASE `"+tenantDB+"`"); err != nil {
					t.Fatalf("drop tenant database: %v", err)
				}
				// Reset cannot be what saves this: it is best-effort and its
				// caller discards the error, so skip it the way a canceled
				// request does.
				conn.Close()
			} else {
				conn.Close()
				if _, err := pool.Exec(ctx, "DROP DATABASE `"+tenantDB+"`"); err != nil {
					t.Fatalf("drop tenant database: %v", err)
				}
			}

			// The next request, belonging to no tenant of its own.
			next, err := pool.Conn(ctx)
			if err != nil {
				t.Fatalf("acquire conn for the next request: %v", err)
			}
			defer next.Close()

			if err := defaultReq.Apply(ctx, next); err != nil {
				t.Fatalf("apply default-tenant isolation: %v", err)
			}

			var current sql.NullString
			if err := next.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
				t.Fatalf("SELECT DATABASE(): %v", err)
			}
			if current.String != engineDB {
				t.Errorf("next request is on database %q, want %q", current.String, engineDB)
			}

			// The symptom: an unqualified reference to a table in the engine's
			// own database.
			var owner string
			if err := next.QueryRowContext(ctx, "SELECT owner FROM tenancy_probe").Scan(&owner); err != nil {
				t.Errorf("unqualified query on the next request: %v", err)
			}
		})
	}
}

// Apply has to leave the connection on a known database whatever it found,
// which is the guarantee the Postgres path gets from re-running SET
// search_path on every acquisition. Reset is not that guarantee: it is
// documented best-effort and its caller throws the error away, so a connection
// can reach the next request still pinned to the previous request's tenant.
//
// On MySQL that surfaces as an error whenever the tenant's tables differ from
// the engine's. When they do not differ, on either engine, the query succeeds
// against the wrong tenant's data and nothing is logged at all, which is why
// the probe table exists in both databases with different contents.
func TestDatabaseTenancy_ApplyRepinsAConnectionLeftOnAnotherTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	for _, tc := range []struct {
		engine      string
		pool        func(*testing.T) db.DB
		tenancy     func(func(context.Context) string, string) db.Tenancy
		createDB    string
		dropDB      string
		createProbe string
		whichDB     string
	}{
		{
			engine:      "mysql",
			pool:        testdb.MySQL,
			tenancy:     db.NewMySQLDatabaseTenancy,
			createDB:    "CREATE DATABASE `tenant_%[1]s`",
			dropDB:      "DROP DATABASE IF EXISTS `tenant_%[1]s`",
			createProbe: "CREATE TABLE %[1]s.tenancy_probe (owner VARCHAR(64))",
			whichDB:     "SELECT DATABASE()",
		},
		{
			engine:   "mssql",
			pool:     testdb.MSSQL,
			tenancy:  db.NewMSSQLDatabaseTenancy,
			createDB: "CREATE DATABASE [tenant_%[1]s]",
			dropDB: "IF DB_ID(N'tenant_%[1]s') IS NOT NULL BEGIN " +
				"ALTER DATABASE [tenant_%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE " +
				"DROP DATABASE [tenant_%[1]s] END",
			createProbe: "CREATE TABLE [%[1]s].dbo.tenancy_probe (owner NVARCHAR(64))",
			whichDB:     "SELECT DB_NAME()",
		},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			pool := tc.pool(t)
			ctx := context.Background()

			engineDB := db.DatabaseNameOf(pool)
			if engineDB == "" {
				t.Fatal("pool did not report its own database name")
			}

			slug := fmt.Sprintf("repin_%d", time.Now().UnixNano()%100000)
			if _, err := pool.Exec(ctx, fmt.Sprintf(tc.createDB, slug)); err != nil {
				t.Fatalf("create tenant database: %v", err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), fmt.Sprintf(tc.dropDB, slug))
			})

			// The same table in both databases, so that a connection left on
			// the tenant answers the query instead of failing it.
			for owner, target := range map[string]string{
				"engine": engineDB,
				"tenant": "tenant_" + slug,
			} {
				if _, err := pool.Exec(ctx, fmt.Sprintf(tc.createProbe, target)); err != nil {
					t.Fatalf("create probe table in %s: %v", target, err)
				}
				if _, err := pool.Exec(ctx, "INSERT INTO "+qualify(tc.engine, target)+" VALUES ('"+owner+"')"); err != nil {
					t.Fatalf("seed probe table in %s: %v", target, err)
				}
			}

			conn, err := pool.Conn(ctx)
			if err != nil {
				t.Fatalf("acquire conn: %v", err)
			}
			defer conn.Close()

			tenantReq := tc.tenancy(func(context.Context) string { return slug }, engineDB)
			if err := tenantReq.Apply(ctx, conn); err != nil {
				t.Fatalf("apply tenant isolation: %v", err)
			}

			// No Reset, standing in for the one that failed or never ran.
			defaultReq := tc.tenancy(func(context.Context) string { return "default" }, engineDB)
			if err := defaultReq.Apply(ctx, conn); err != nil {
				t.Fatalf("apply default-tenant isolation: %v", err)
			}

			var current sql.NullString
			if err := conn.QueryRowContext(ctx, tc.whichDB).Scan(&current); err != nil {
				t.Fatalf("read current database: %v", err)
			}
			if current.String != engineDB {
				t.Errorf("connection is on database %q, want %q", current.String, engineDB)
			}

			var owner string
			if err := conn.QueryRowContext(ctx, "SELECT owner FROM tenancy_probe").Scan(&owner); err != nil {
				t.Fatalf("unqualified query after re-pinning: %v", err)
			}
			if owner != "engine" {
				t.Errorf("the default-tenant request read %q data, want the engine's own", owner)
			}
		})
	}
}

// qualify names the probe table from outside the database holding it.
func qualify(engine, database string) string {
	if engine == "mssql" {
		return "[" + database + "].dbo.tenancy_probe"
	}
	return "`" + database + "`.tenancy_probe"
}
