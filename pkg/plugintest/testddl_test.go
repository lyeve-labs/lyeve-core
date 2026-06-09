package plugintest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// probeDDL creates table on each dialect, guarded so it can run on a
// database that already has it.
func probeDDL(table string) func(dialect string) []string {
	return func(dialect string) []string {
		switch dialect {
		case "postgres", "mysql":
			return []string{fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id INT NOT NULL PRIMARY KEY)", table)}
		case "mssql":
			return []string{fmt.Sprintf("IF OBJECT_ID(N'%s', N'U') IS NULL CREATE TABLE %s (id INT NOT NULL PRIMARY KEY)", table, table)}
		}
		return nil
	}
}

// auditIndexDDL declares the audit table and one of its indexes the way a
// suite would. Two owners register it, so on MySQL the second index statement
// meets an index that exists.
func auditIndexDDL(dialect string) []string {
	switch dialect {
	case "postgres":
		return []string{
			`CREATE TABLE IF NOT EXISTS sys_audit_log (id UUID PRIMARY KEY, ts TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
			`CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON sys_audit_log(ts DESC)`,
		}
	case "mysql":
		return []string{
			"CREATE TABLE IF NOT EXISTS sys_audit_log (`id` CHAR(36) PRIMARY KEY, `ts` DATETIME(6) NOT NULL DEFAULT NOW(6))",
			"CREATE INDEX idx_audit_log_ts ON sys_audit_log(`ts` DESC)",
		}
	case "mssql":
		return []string{
			`IF OBJECT_ID(N'sys_audit_log', N'U') IS NULL CREATE TABLE sys_audit_log (id UNIQUEIDENTIFIER PRIMARY KEY, ts DATETIME2(7) NOT NULL DEFAULT SYSUTCDATETIME())`,
			`IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_audit_log_ts' AND object_id = OBJECT_ID('sys_audit_log'))
CREATE INDEX idx_audit_log_ts ON sys_audit_log(ts DESC)`,
		}
	}
	return nil
}

func init() {
	plugintest.RegisterTestDDL("probe-init", probeDDL("plugintest_probe_init"))
	plugintest.RegisterTestDDL("audit", auditIndexDDL)
	plugintest.RegisterTestDDL("audit-again", auditIndexDDL)
}

// assertTableUsable writes a row to table and reads it back through the host,
// which fails unless the table exists on the host's database.
func assertTableUsable(t *testing.T, host core.Host, table string) {
	t.Helper()
	ctx := context.Background()
	_, err := host.Querier(ctx).Exec(ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES ($1)", table), 7)
	require.NoError(t, err, "insert into %s on %s", table, host.Dialect())

	row, err := host.Querier(ctx).QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = $1", table), 7)
	require.NoError(t, err)
	var n int
	require.NoError(t, row.Scan(&n))
	assert.Equal(t, 1, n, "%s on %s", table, host.Dialect())
}

func hostFor(t *testing.T, dialect string) core.Host {
	t.Helper()
	switch dialect {
	case "postgres":
		return plugintest.Postgres(t)
	case "mysql":
		return plugintest.MySQL(t)
	case "mssql":
		return plugintest.MSSQL(t)
	}
	t.Fatalf("no host for dialect %q", dialect)
	return nil
}

// A table registered before the first database exists is on every test
// database, which on Postgres means it was built into the template.
func TestRegisterTestDDL_TableIsOnEveryTestDatabase(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		assertTableUsable(t, host, "plugintest_probe_init")
	})
}

// A registration made after databases already exist reaches every database
// created after it, though the Postgres template was built without it.
func TestRegisterTestDDL_ReachesDatabasesCreatedAfterIt(t *testing.T) {
	for _, dialect := range plugintest.DialectNames() {
		hostFor(t, dialect)
	}

	plugintest.RegisterTestDDL("probe-late", probeDDL("plugintest_probe_late"))

	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		assertTableUsable(t, host, "plugintest_probe_late")
	})
}

// A second registration under an owner is ignored, so suites that each
// declare one plugin's tables do not run the statements twice.
func TestRegisterTestDDL_AnOwnerRegistersOnce(t *testing.T) {
	plugintest.RegisterTestDDL("probe-init", probeDDL("plugintest_probe_ignored"))

	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		fixture := plugintest.NewDBFixture(t, host)
		assert.True(t, fixture.TableExists(context.Background(), "plugintest_probe_init"))
		assert.False(t, fixture.TableExists(context.Background(), "plugintest_probe_ignored"))
	})
}

// Two suites of one binary can declare the same table under their own owner
// names. The Postgres and SQL Server statements guard themselves, and on
// MySQL the index that already exists is skipped rather than failing every
// database.
func TestRegisterTestDDL_TwoOwnersDeclareOneTable(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		fixture := plugintest.NewDBFixture(t, host)
		assert.True(t, fixture.TableExists(context.Background(), "sys_audit_log"))
		assert.Equal(t, 0, fixture.CountRows(context.Background(), "sys_audit_log"))
	})
}
