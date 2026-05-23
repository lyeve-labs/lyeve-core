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

// A tenant gets no database of its own on the database-per-tenant engines, so
// the strategy has to put its requests somewhere. The engine's own database is
// where every table the tenant can read lives: the pool names it on each sys_*
// reference and the generated content tables carry the same qualifier, so a
// connection left inside tenant_<slug> would find nothing there to read.

func TestMySQLTenancy_TenantWithoutADatabaseRunsOnTheEngineDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	engineDB := db.DatabaseNameOf(pool)

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Close()

	slug := uniqueSlug("nodb")
	tenancy := db.NewMySQLDatabaseTenancy(func(context.Context) string { return slug }, engineDB)

	if err := tenancy.Apply(ctx, conn); err != nil {
		t.Fatalf("Apply for a tenant with no database: %v", err)
	}

	var current string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
		t.Fatalf("SELECT DATABASE(): %v", err)
	}
	if current != engineDB {
		t.Errorf("DATABASE() = %q, want the engine's own %q", current, engineDB)
	}

	assertReachesCatalog(t, pool, conn)
}

func TestMSSQLTenancy_TenantWithoutADatabaseRunsOnTheEngineDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()
	engineDB := db.DatabaseNameOf(pool)

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Close()

	slug := uniqueSlug("nodb")
	tenancy := db.NewMSSQLDatabaseTenancy(func(context.Context) string { return slug }, engineDB)

	if err := tenancy.Apply(ctx, conn); err != nil {
		t.Fatalf("Apply for a tenant with no database: %v", err)
	}

	var current string
	if err := conn.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&current); err != nil {
		t.Fatalf("SELECT DB_NAME(): %v", err)
	}
	if current != engineDB {
		t.Errorf("DB_NAME() = %q, want the engine's own %q", current, engineDB)
	}

	assertReachesCatalog(t, pool, conn)
}

// The failed USE must leave the connection usable. A driver that poisoned the
// session on a missing database would turn the fall-back into an outage for
// every request that followed it onto that pooled connection.
func TestMySQLTenancy_ConnectionSurvivesTheMissingDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	assertConnectionSurvives(t, testdb.MySQL(t), "mysql")
}

func TestMSSQLTenancy_ConnectionSurvivesTheMissingDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	assertConnectionSurvives(t, testdb.MSSQL(t), "mssql")
}

// A tenant that does have a database keeps using it. An install provisioned by
// a clone or an import that put tables there must not silently start reading
// somewhere else.
func TestMySQLTenancy_ExistingTenantDatabaseIsUsed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	slug := uniqueSlug("provisioned")
	name := "tenant_" + slug

	if _, err := pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+name+"`"); err != nil {
		t.Fatalf("create tenant database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
	})

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Close()

	tenancy := db.NewMySQLDatabaseTenancy(func(context.Context) string { return slug }, db.DatabaseNameOf(pool))
	if err := tenancy.Apply(ctx, conn); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var current string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
		t.Fatalf("SELECT DATABASE(): %v", err)
	}
	if current != name {
		t.Errorf("DATABASE() = %q, want %q", current, name)
	}
}

func TestMSSQLTenancy_ExistingTenantDatabaseIsUsed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()
	slug := uniqueSlug("provisioned")
	name := "tenant_" + slug

	if _, err := pool.Exec(ctx, "IF DB_ID(N'"+name+"') IS NULL CREATE DATABASE ["+name+"]"); err != nil {
		t.Fatalf("create tenant database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"IF DB_ID(N'"+name+"') IS NOT NULL BEGIN ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE ["+name+"] END")
	})

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Close()

	tenancy := db.NewMSSQLDatabaseTenancy(func(context.Context) string { return slug }, db.DatabaseNameOf(pool))
	if err := tenancy.Apply(ctx, conn); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var current string
	if err := conn.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&current); err != nil {
		t.Fatalf("SELECT DB_NAME(): %v", err)
	}
	if current != name {
		t.Errorf("DB_NAME() = %q, want %q", current, name)
	}
}

// Once a slug has been found to have no database, the strategy stops asking:
// the statement that cannot succeed is worth one round trip per slug, not one
// per request. A database that appears afterwards is not picked up until the
// process restarts, which is safe because nothing a request reads on these
// engines resolves inside a tenant database.
func TestMySQLTenancy_MissingDatabaseIsAskedAboutOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	engineDB := db.DatabaseNameOf(pool)
	slug := uniqueSlug("once")
	name := "tenant_" + slug

	tenancy := db.NewMySQLDatabaseTenancy(func(context.Context) string { return slug }, engineDB)

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Close()

	if err := tenancy.Apply(ctx, conn); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	if _, err := pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+name+"`"); err != nil {
		t.Fatalf("create tenant database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
	})

	if err := tenancy.Apply(ctx, conn); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	var current string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil {
		t.Fatalf("SELECT DATABASE(): %v", err)
	}
	if current != engineDB {
		t.Errorf("DATABASE() = %q, want the engine's own %q: the answer is meant to be remembered", current, engineDB)
	}
}

// assertReachesCatalog runs an unqualified catalog read through the pool
// wrapper on a tenant-scoped connection. It is the whole point of the
// fall-back: a request for a tenant with no database of its own still reaches
// the catalog, which is where its rows are.
func assertReachesCatalog(t *testing.T, pool db.DB, conn *sql.Conn) {
	t.Helper()
	ctx := db.WithTenantConn(context.Background(), conn)
	row, err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM sys_users")
	if err != nil {
		t.Fatalf("QueryRow on a tenant-scoped conn: %v", err)
	}
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("read sys_users on a tenant-scoped conn: %v", err)
	}
}

// assertConnectionSurvives proves the connection is still usable after the USE
// that could not succeed.
func assertConnectionSurvives(t *testing.T, pool db.DB, engine string) {
	t.Helper()
	ctx := context.Background()
	engineDB := db.DatabaseNameOf(pool)
	slug := uniqueSlug("survive")

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Close()

	var tenancy db.Tenancy
	if engine == "mysql" {
		tenancy = db.NewMySQLDatabaseTenancy(func(context.Context) string { return slug }, engineDB)
	} else {
		tenancy = db.NewMSSQLDatabaseTenancy(func(context.Context) string { return slug }, engineDB)
	}
	if err := tenancy.Apply(ctx, conn); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var one int
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("the connection did not survive the missing database: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 = %d", one)
	}
}

func uniqueSlug(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1000000)
}
