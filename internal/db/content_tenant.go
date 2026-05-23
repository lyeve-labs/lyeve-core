package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	mssql "github.com/microsoft/go-mssqldb"
)

// TenantColumnName is the tenant-isolation column every generated content and
// pivot table carries. The tables themselves are shared across tenants (they
// live in one schema/database, like sys_*), so this column is the isolation
// boundary: every content query filters by it and every insert writes it.
const TenantColumnName = "tenant_id"

// TenantColumnType returns the SQL type of the tenant_id column for the given
// engine. It must match the type the schema engine gives the same column.
// The tenant slug is at most 63 characters, so the bounded widths suffice.
func TenantColumnType(engine string) string {
	switch engine {
	case "mysql":
		return "VARCHAR(64)"
	case "mssql":
		return "NVARCHAR(64)"
	default:
		return "TEXT"
	}
}

// TenantColumnDef renders the tenant_id column definition for generated tables.
// The identifier is quoted for the engine. The NOT NULL DEFAULT backfills
// pre-existing rows with the empty tenant bucket at ALTER time.
func TenantColumnDef(engine string) string {
	return fmt.Sprintf("%s %s NOT NULL DEFAULT ''",
		dialect.Must(engine).QuoteIdentifier(TenantColumnName), TenantColumnType(engine))
}

// qualifyGeneratedTable returns the runtime SQL reference for a generated
// content or pivot table. A tenant-scoped connection on MySQL/MSSQL is switched
// to the tenant's own database, so the reference must name the engine's
// database explicitly to reach the shared table: the same reason sys_*
// references are qualified. Postgres resolves the shared table through
// search_path, so its reference stays bare. An empty or invalid database name
// disables qualification. Table names come from domain.ValidateIdentifier.
func qualifyGeneratedTable(engine, dbName, table string) string {
	switch engine {
	case "mysql":
		if safeDatabaseNameRe.MatchString(dbName) {
			return quoteMySQLIdentifier(dbName) + "." + quoteMySQLIdentifier(table)
		}
	case "mssql":
		if safeDatabaseNameRe.MatchString(dbName) {
			return quoteMSSQLIdentifier(dbName) + ".." + quoteMSSQLIdentifier(table)
		}
	}
	return table
}

// AddTenantColumnIfMissingSQL returns idempotent DDL that adds the tenant_id
// column to an existing generated table. ref is the runtime table reference
// (already engine-qualified). The bare argument is the unquoted table name
// for the catalog checks. The dbName argument names the engine's own
// database, which the catalog guards must target: on a tenant-scoped
// connection the current database is the tenant's, where the shared table
// does not exist. MySQL 8.0 has no ADD
// COLUMN IF NOT EXISTS, so the whole ALTER is guarded by a PREPARE block in
// one multi-statement Exec (multiStatements is enabled on the engine DSN).
// MSSQL guards on sys.columns.
func AddTenantColumnIfMissingSQL(engine, ref, bare, dbName string) string {
	colDef := TenantColumnDef(engine)
	qualified := dbName != "" && safeDatabaseNameRe.MatchString(dbName)
	switch engine {
	case "mysql":
		scope := "DATABASE()"
		if qualified {
			scope = "'" + dbName + "'"
		}
		// The ALTER text lands inside a string literal, so embedded single
		// quotes must be doubled to survive the assignment.
		return fmt.Sprintf(
			"SET @lyeve_col = (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = %s AND table_name = '%s' AND column_name = '%s');\n"+
				"SET @lyeve_sql = IF(@lyeve_col = 0, 'ALTER TABLE %s ADD COLUMN %s', 'DO 0');\n"+
				"PREPARE lyeve_tenant_stmt FROM @lyeve_sql;\n"+
				"EXECUTE lyeve_tenant_stmt;\n"+
				"DEALLOCATE PREPARE lyeve_tenant_stmt;",
			scope, bare, TenantColumnName, ref, strings.ReplaceAll(colDef, "'", "''"),
		)
	case "mssql":
		obj := bare
		cat := "sys.columns"
		if qualified {
			obj = dbName + ".." + bare
			// sys.columns is database-scoped: on a tenant connection the
			// catalog view must be addressed in the engine's own database.
			cat = quoteMSSQLIdentifier(dbName) + ".sys.columns"
		}
		return fmt.Sprintf(
			"IF NOT EXISTS (SELECT 1 FROM %s WHERE name = N'%s' AND object_id = OBJECT_ID(N'%s')) ALTER TABLE %s ADD %s",
			cat, TenantColumnName, obj, ref, colDef,
		)
	default:
		return fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s", ref, colDef)
	}
}

// EnsureTenantColumn adds the tenant_id column to a generated table when it is
// missing, on the pool's current scope. Idempotent. Safe to call per request
// for handlers that touch a table outside the ContentStore paths. Concurrent
// instances booting against the same database race on the conditional ALTER,
// and the loser reports a duplicate column: that outcome is success.
func EnsureTenantColumn(ctx context.Context, pool DB, table string) error {
	dbName := DatabaseNameOf(pool)
	ref := qualifyGeneratedTable(pool.Engine(), dbName, table)
	sql := AddTenantColumnIfMissingSQL(pool.Engine(), ref, table, dbName)
	if _, err := pool.Exec(ctx, sql); err != nil {
		if isDuplicateColumnError(err, pool.Engine()) {
			return nil
		}
		return fmt.Errorf("ensure tenant column on %s: %w", table, err)
	}
	return nil
}

// isDuplicateColumnError reports whether err is the engine's duplicate-column
// failure: two instances raced on the conditional ALTER and the loser lost.
func isDuplicateColumnError(err error, engine string) bool {
	switch engine {
	case "mysql":
		var myErr *mysql.MySQLError
		return errors.As(err, &myErr) && myErr.Number == 1060
	case "mssql":
		var msErr mssql.Error
		return errors.As(err, &msErr) && msErr.Number == 2705
	default:
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "42701"
	}
}
