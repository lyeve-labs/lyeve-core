package sqlx

import (
	"errors"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// mssqlMissingObjectNumbers are the SQL Server errors that report a named table
// the statement could not resolve.
//
// SQL Server does not use one number for this. A SELECT, INSERT, UPDATE or
// DELETE naming a table that is gone reports 208, but the DDL statements report
// their own: ALTER TABLE gives 4902, CREATE INDEX 1088 and TRUNCATE TABLE 4701,
// all with the same "Cannot find the object" text. Matching only 208 would
// leave the DDL forms unclassified, and a read that first tries a conditional
// ALTER on a dropped table would answer "the database is unavailable" instead
// of "that table is gone".
var mssqlMissingObjectNumbers = map[int32]bool{
	208:  true, // invalid object name (DML and SELECT)
	1088: true, // CREATE INDEX
	4701: true, // TRUNCATE TABLE
	4902: true, // ALTER TABLE
}

// IsUndefinedTable reports whether err says the table does not exist, matched
// via each driver's native error type rather than message text:
//
//   - PostgreSQL (pgx): pgconn.PgError with Code "42P01"
//   - MySQL: mysql.MySQLError with Number 1146
//   - MSSQL: mssql.Error with one of mssqlMissingObjectNumbers
//
// A caller that sweeps a list of tables it did not build itself meets this
// whenever one of them has since been dropped. That is an answer, not a
// failure: a table that is gone holds nothing. Telling it apart from a probe
// that genuinely could not run is what keeps a routine outcome out of the
// warning log, where enough of it will bury a real fault.
func IsUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return true
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == 1146 {
		return true
	}
	// Value receiver, so the wrapped error is an mssql.Error, never a pointer.
	var msErr mssql.Error
	if errors.As(err, &msErr) && mssqlMissingObjectNumbers[msErr.Number] {
		return true
	}
	return false
}
