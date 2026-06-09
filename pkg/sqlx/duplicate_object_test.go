package sqlx

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

func TestIsDuplicateObject(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"postgres duplicate column", &pgconn.PgError{Code: "42701"}, true},
		{"postgres duplicate table", &pgconn.PgError{Code: "42P07"}, true},
		{"postgres duplicate index", &pgconn.PgError{Code: "42710"}, true},
		{"postgres missing table", &pgconn.PgError{Code: "42P01"}, false},
		{"mysql duplicate column", &mysql.MySQLError{Number: 1060}, true},
		{"mysql table exists", &mysql.MySQLError{Number: 1050}, true},
		{"mysql duplicate row", &mysql.MySQLError{Number: 1062}, false},
		{"mysql missing table", &mysql.MySQLError{Number: 1146}, false},
		{"mssql object exists", mssql.Error{Number: 2714}, true},
		{"mssql duplicate column", mssql.Error{Number: 2705}, true},
		{"mssql missing object", mssql.Error{Number: 208}, false},
		{"unrelated", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDuplicateObject(tc.err); got != tc.want {
				t.Fatalf("IsDuplicateObject(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The classifier has to survive the wrapping every store does on the way up,
// because the caller that asks the question is several frames above the driver.
func TestIsDuplicateObject_ThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("exec 003_tenant_id: %w",
		fmt.Errorf("apply migration: %w", &pgconn.PgError{Code: "42701"}))
	if !IsDuplicateObject(wrapped) {
		t.Fatal("a wrapped duplicate-column error must still classify")
	}
}
