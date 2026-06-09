package httpx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// pgError stands in for pgconn.PgError, which StoreStatusFor reads through the
// SQLState method rather than by importing the driver.
type pgError struct{ code string }

func (e *pgError) Error() string    { return "pg: " + e.code }
func (e *pgError) SQLState() string { return e.code }

// mssqlError stands in for go-mssqldb's Error, read through SQLErrorNumber.
type mssqlError struct{ number int32 }

func (e mssqlError) Error() string           { return fmt.Sprintf("mssql: %d", e.number) }
func (e mssqlError) SQLErrorNumber() int32   { return e.number }
func (e mssqlError) SQLErrorClass() uint8    { return 16 }
func (e mssqlError) SQLErrorMessage() string { return e.Error() }

// Posting a duplicate to a unique field, or a string to a numeric one, is the
// caller's to fix. Answered with a 503, a client retries forever and a 5xx
// monitor reads a healthy engine as down.
func TestStoreStatusFor_CallerErrorsAreNot503(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"postgres duplicate key", &pgError{"23505"}, http.StatusConflict},
		{"postgres not null", &pgError{"23502"}, http.StatusUnprocessableEntity},
		{"postgres foreign key", &pgError{"23503"}, http.StatusUnprocessableEntity},
		{"postgres check constraint", &pgError{"23514"}, http.StatusUnprocessableEntity},
		{"postgres string is not a number", &pgError{"22P02"}, http.StatusUnprocessableEntity},
		{"postgres numeric overflow", &pgError{"22003"}, http.StatusUnprocessableEntity},
		{"postgres value too long", &pgError{"22001"}, http.StatusUnprocessableEntity},

		{"mysql duplicate entry", &mysql.MySQLError{Number: 1062}, http.StatusConflict},
		{"mysql column cannot be null", &mysql.MySQLError{Number: 1048}, http.StatusUnprocessableEntity},
		{"mysql out of range", &mysql.MySQLError{Number: 1264}, http.StatusUnprocessableEntity},
		{"mysql data too long", &mysql.MySQLError{Number: 1406}, http.StatusUnprocessableEntity},
		{"mysql no parent row", &mysql.MySQLError{Number: 1452}, http.StatusUnprocessableEntity},

		{"mssql unique constraint", mssqlError{2627}, http.StatusConflict},
		{"mssql duplicate key index", mssqlError{2601}, http.StatusConflict},
		// Adding a UNIQUE field to a schema whose rows already hold duplicates
		// fails here, not on an insert. Postgres reports 23505 and MySQL 1062,
		// both a conflict, and SQL Server's 1505 must answer the same 409. A 503
		// would tell the caller to retry a migration that can never succeed.
		{"mssql create unique index on duplicate data", mssqlError{1505}, http.StatusConflict},
		{"mssql duplicate key index", mssqlError{2601}, http.StatusConflict},
		{"mssql cannot insert null", mssqlError{515}, http.StatusUnprocessableEntity},
		{"mssql constraint violated", mssqlError{547}, http.StatusUnprocessableEntity},
		{"mssql conversion failed", mssqlError{8114}, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StoreStatusFor(tt.err); got != tt.want {
				t.Errorf("StoreStatusFor(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// The wrapping stores apply must not hide the classification.
func TestStoreStatusFor_ClassifiesThroughWrapping(t *testing.T) {
	err := fmt.Errorf("insert content: %w", fmt.Errorf("exec: %w", &pgError{"23505"}))
	if got := StoreStatusFor(err); got != http.StatusConflict {
		t.Errorf("StoreStatusFor(wrapped duplicate) = %d, want %d", got, http.StatusConflict)
	}
}

// Handlers may pass a store error to either mapper, so an error whose kind is
// recognizable gets the same status from both. A database that is merely
// unreachable must not read as a 500 from StatusFor.
func TestStatusFor_AgreesWithStoreStatusForOnKnownErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"connection done", sql.ErrConnDone, http.StatusServiceUnavailable},
		{"transaction done", sql.ErrTxDone, http.StatusServiceUnavailable},
		{"connection closed", net.ErrClosed, http.StatusServiceUnavailable},
		{"deadline exceeded", context.DeadlineExceeded, http.StatusServiceUnavailable},
		{"postgres cannot connect", &pgError{"08006"}, http.StatusServiceUnavailable},
		{"postgres admin shutdown", &pgError{"57P01"}, http.StatusServiceUnavailable},
		{"mysql server gone away", &mysql.MySQLError{Number: 2006}, http.StatusServiceUnavailable},
		{"postgres duplicate key", &pgError{"23505"}, http.StatusConflict},
		{"postgres string is not a number", &pgError{"22P02"}, http.StatusUnprocessableEntity},
		{"mysql duplicate entry", &mysql.MySQLError{Number: 1062}, http.StatusConflict},
		{"mssql unique constraint", mssqlError{2627}, http.StatusConflict},
		{"mssql duplicate key index", mssqlError{2601}, http.StatusConflict},
		// Adding a UNIQUE field to a schema whose rows already hold duplicates
		// fails here, not on an insert. Postgres reports 23505 and MySQL 1062,
		// both a conflict, and SQL Server's 1505 must answer the same 409. A 503
		// would tell the caller to retry a migration that can never succeed.
		{"mssql create unique index on duplicate data", mssqlError{1505}, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusFor(tt.err); got != tt.want {
				t.Errorf("StatusFor(%v) = %d, want %d", tt.err, got, tt.want)
			}
			if got := StoreStatusFor(tt.err); got != tt.want {
				t.Errorf("StoreStatusFor(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// A query the engine itself got wrong is not an outage and not the caller's to
// fix. Leaving class 42 unclassified keeps it at StatusFor's 500, where it reads
// as the bug it is instead of inviting a client to retry forever.
func TestStatusFor_EngineBugsStay500(t *testing.T) {
	for _, state := range []string{
		"42601", // syntax error
		"42P01", // undefined table
		"42703", // undefined column
		"42501", // insufficient privilege
	} {
		t.Run(state, func(t *testing.T) {
			if got := StatusFor(&pgError{state}); got != http.StatusInternalServerError {
				t.Errorf("StatusFor(pg %s) = %d, want 500", state, got)
			}
		})
	}
}

// The mappers part only on an error neither recognizes: a store failure is
// assumed to be the database, anything else is assumed to be a bug.
func TestStatusFor_DiffersFromStoreStatusForOnlyOnUnknownErrors(t *testing.T) {
	err := errors.New("something unexpected")
	if got := StatusFor(err); got != http.StatusInternalServerError {
		t.Errorf("StatusFor(unknown) = %d, want 500", got)
	}
	if got := StoreStatusFor(err); got != http.StatusServiceUnavailable {
		t.Errorf("StoreStatusFor(unknown) = %d, want 503", got)
	}
}

// A genuine outage still has to read as one. Classifying caller errors must not
// pull infrastructure failures out of 503.
func TestStoreStatusFor_InfrastructureStays503(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"connection done", sql.ErrConnDone},
		{"transaction done", sql.ErrTxDone},
		{"postgres cannot connect", &pgError{"08006"}},
		{"postgres too many connections", &pgError{"53300"}},
		{"postgres admin shutdown", &pgError{"57P01"}},
		{"mysql server gone away", &mysql.MySQLError{Number: 2006}},
		{"mysql too many connections", &mysql.MySQLError{Number: 1040}},
		{"mssql deadlock victim", mssqlError{1205}},
		{"unrecognized error", errors.New("something unexpected")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StoreStatusFor(tt.err); got != http.StatusServiceUnavailable {
				t.Errorf("StoreStatusFor(%v) = %d, want 503", tt.err, got)
			}
		})
	}
}

// TestClassify_CoreSentinels pins the status of each core sentinel. Plugins
// map their store errors through these mappers rather than their own helpers,
// so a duplicate key and a dropped connection stay distinguishable.
func TestClassify_CoreSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", core.ErrNotFound, http.StatusNotFound},
		{"conflict", core.ErrConflict, http.StatusConflict},
		{"forbidden", core.ErrForbidden, http.StatusForbidden},
		{"unauthorized", core.ErrUnauth, http.StatusUnauthorized},
		{"service unavailable", core.ErrServiceUnavailable, http.StatusServiceUnavailable},
		{"tenant required", core.ErrTenantRequired, http.StatusBadRequest},
		{"wrapped tenant required", fmt.Errorf("store.Create: %w", core.ErrTenantRequired), http.StatusBadRequest},
		{"wrapped forbidden", fmt.Errorf("delete hook: %w", core.ErrForbidden), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StatusFor(tc.err); got != tc.want {
				t.Errorf("StatusFor = %d, want %d", got, tc.want)
			}
			if got := StoreStatusFor(tc.err); got != tc.want {
				t.Errorf("StoreStatusFor = %d, want %d", got, tc.want)
			}
		})
	}
}

// A tenant slug the engine refuses is not an outage. Unclassified, it would
// take StoreStatusFor's 503 default, which tells the caller to retry a request
// that will be refused identically every time, and makes a bad slug in a token
// indistinguishable from the database being down.
func TestStoreStatusFor_ValidationIsNotAnOutage(t *testing.T) {
	err := fmt.Errorf("%w: invalid tenant slug: %q", core.ErrValidation, "Bad Slug!")

	if got := StoreStatusFor(err); got != http.StatusBadRequest {
		t.Errorf("StoreStatusFor = %d; want 400", got)
	}
	if got := StatusFor(err); got != http.StatusBadRequest {
		t.Errorf("StatusFor = %d; want 400", got)
	}
}
