package sqlx

import (
	"errors"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// pgDuplicateObjectCodes are the PostgreSQL states for DDL that would create a
// name the schema already holds. They are separate states rather than one,
// because the standard classes the object kinds apart.
var pgDuplicateObjectCodes = map[string]bool{
	"42P07": true, // duplicate_table
	"42701": true, // duplicate_column
	"42P06": true, // duplicate_schema
	"42710": true, // duplicate_object, which covers an index or a constraint
}

// mysqlDuplicateObjectNumbers are the MySQL errors for the same thing. The
// duplicate-row numbers (1022 and 1062) are deliberately absent: those report
// data a UNIQUE index refused, which is a different fault with a different
// repair.
var mysqlDuplicateObjectNumbers = map[uint16]bool{
	1050: true, // table already exists
	1060: true, // duplicate column name
	1061: true, // duplicate key name
	1826: true, // duplicate foreign key constraint name
}

// mssqlDuplicateObjectNumbers are the SQL Server equivalents. As with the
// missing-object numbers, SQL Server reports the kinds under separate numbers
// rather than one.
var mssqlDuplicateObjectNumbers = map[int32]bool{
	2714: true, // there is already an object named X in the database
	2705: true, // column names in each table must be unique
	1913: true, // an index with that name already exists on the object
}

// IsDuplicateObject reports whether err says the DDL would create a name the
// schema already has, matched by each driver's native error type rather than
// message text.
//
// This is the signature of a migration running a second time. On its own that
// says nothing about which of the two causes it is, and the caller has to
// supply the rest: a migration whose script creates something an earlier
// migration in the same tree already created is broken, while one whose
// bookkeeping row went missing is a sound migration against an install that
// lost its record. Only the caller knows which of those it is looking at.
func IsDuplicateObject(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgDuplicateObjectCodes[pgErr.Code] {
		return true
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && mysqlDuplicateObjectNumbers[myErr.Number] {
		return true
	}
	// Value receiver, so the wrapped error is an mssql.Error, never a pointer.
	var msErr mssql.Error
	if errors.As(err, &msErr) && mssqlDuplicateObjectNumbers[msErr.Number] {
		return true
	}
	return false
}
