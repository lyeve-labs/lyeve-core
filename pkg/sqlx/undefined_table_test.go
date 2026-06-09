package sqlx_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

func TestIsUndefinedTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"postgres undefined table", &pgconn.PgError{Code: "42P01"}, true},
		{"postgres undefined column", &pgconn.PgError{Code: "42703"}, false},
		{"mysql no such table", &mysql.MySQLError{Number: 1146}, true},
		{"mysql unknown database", &mysql.MySQLError{Number: 1049}, false},
		{"mssql invalid object name", mssql.Error{Number: 208}, true},
		{"mssql alter table missing object", mssql.Error{Number: 4902}, true},
		{"mssql create index missing object", mssql.Error{Number: 1088}, true},
		{"mssql truncate missing object", mssql.Error{Number: 4701}, true},
		{"mssql deadlock victim", mssql.Error{Number: 1205}, false},
		{"mssql invalid column name", mssql.Error{Number: 207}, false},
		{"unrelated", errors.New("connection refused"), false},
		{"wrapped postgres", fmt.Errorf("probe: %w", &pgconn.PgError{Code: "42P01"}), true},
		{"wrapped mysql", fmt.Errorf("probe: %w", &mysql.MySQLError{Number: 1146}), true},
		{"wrapped mssql", fmt.Errorf("probe: %w", mssql.Error{Number: 208}), true},
		{"wrapped mssql alter table", fmt.Errorf("ensure tenant column: %w", mssql.Error{Number: 4902}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sqlx.IsUndefinedTable(tc.err); got != tc.want {
				t.Errorf("IsUndefinedTable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
