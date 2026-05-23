package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// MySQL reports how many rows an UPDATE changed. Postgres and SQL Server report
// how many it matched. Callers read RowsAffected == 0 as "no such row", so on
// MySQL an update writing the values a row already holds would be
// indistinguishable from an update against a row that is not there.
//
// clientFoundRows makes MySQL count matched rows, which is what those callers
// already assume.
func TestDriverDSN_MySQLCountsMatchedRowsNotChangedOnes(t *testing.T) {
	got := driverDSN("mysql", "mysql://root:secret@tcp(127.0.0.1:3306)/lyeve")
	assert.Contains(t, got, "clientFoundRows=true",
		"without this an idempotent update is reported as a missing row")
	assert.Contains(t, got, "multiStatements=true", "the existing flag must survive")
	assert.False(t, strings.HasPrefix(got, "mysql://"), "the driver rejects the scheme")
}

// An operator who has already chosen a value keeps it.
func TestDriverDSN_MySQLDoesNotOverrideAnExplicitClientFoundRows(t *testing.T) {
	got := driverDSN("mysql", "root:secret@tcp(127.0.0.1:3306)/lyeve?clientFoundRows=false")
	assert.Contains(t, got, "clientFoundRows=false")
	assert.NotContains(t, got, "clientFoundRows=true")
}

// The flag is MySQL-only: the other two engines already count matched rows and
// their DSNs are passed through untouched.
func TestDriverDSN_OtherEnginesAreNotRewritten(t *testing.T) {
	pg := "postgres://u:p@localhost:5432/lyeve?sslmode=disable"
	assert.Equal(t, pg, driverDSN("postgres", pg))

	ms := "sqlserver://sa:p@localhost:1433?database=lyeve"
	assert.Equal(t, ms, driverDSN("mssql", ms))
}
