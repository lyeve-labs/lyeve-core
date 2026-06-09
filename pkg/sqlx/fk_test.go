package sqlx

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

func TestIsForeignKeyViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"postgres 23503", &pgconn.PgError{Code: "23503"}, true},
		{"postgres unique violation is not an fk", &pgconn.PgError{Code: "23505"}, false},
		{"mysql 1451", &mysql.MySQLError{Number: 1451}, true},
		{"mysql 1452", &mysql.MySQLError{Number: 1452}, true},
		{"mysql deadlock is not an fk", &mysql.MySQLError{Number: 1213}, false},
		{"mssql 547", mssql.Error{Number: 547}, true},
		{"mssql deadlock is not an fk", mssql.Error{Number: 1205}, false},
		{"a wrapped violation is still found", fmt.Errorf("record delivery: %w", &pgconn.PgError{Code: "23503"}), true},
		{"a plain error is not", errors.New("boom"), false},
		{"nil is not", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsForeignKeyViolation(tc.err); got != tc.want {
				t.Errorf("IsForeignKeyViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
