package sqlx

import (
	"errors"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// IsForeignKeyViolation reports whether err is a foreign key constraint
// violation, matched via each driver's native error type rather than message
// text:
//
//   - PostgreSQL (pgx): pgconn.PgError with Code "23503"
//   - MySQL: mysql.MySQLError with Number 1451 or 1452
//   - MSSQL: mssql.Error with Number 547
//
// A write that races the deletion of the row it references fails this way on
// every engine. Where the referencing row would have been removed by the
// cascade anyway, that is an expected outcome rather than a fault, and telling
// the two apart is what keeps a routine race out of the error log.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return true
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && (myErr.Number == 1451 || myErr.Number == 1452) {
		return true
	}
	// go-mssqldb's Error carries a value receiver, so the wrapped error is an
	// mssql.Error and never a *mssql.Error: matching the pointer silently fails.
	var msErr mssql.Error
	if errors.As(err, &msErr) && msErr.Number == 547 {
		return true
	}
	return false
}
