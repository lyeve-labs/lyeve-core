package plugintest

import "github.com/lyeve-labs/lyeve-core/internal/testdb"

// RegisterTestDDL declares tables a suite needs that no migration it runs
// creates, such as a table another plugin owns that the code under test
// writes to. The statements run on every test database of the binary, on
// every dialect, after the engine's migrations and before any plugin's, so a
// plugin migration can already refer to the table.
//
// owner names the plugin that owns the tables. ddl returns the statements
// for a dialect, "postgres", "mysql" or "mssql", and nil for a dialect it
// does not cover. Write each statement so it can run on a database that
// already has the table: IF NOT EXISTS on Postgres and on a MySQL table, an
// OBJECT_ID or sys.indexes guard on SQL Server. A MySQL index that already
// exists is skipped, since MySQL cannot guard CREATE INDEX.
//
// An owner registers once, and a later registration under the same owner is
// ignored, so two suites of one binary can both declare one plugin's tables.
// Call it from init or TestMain. A call from inside a test still reaches
// every database created after it.
func RegisterTestDDL(owner string, ddl func(dialect string) []string) {
	testdb.RegisterDDL(owner, ddl)
}
