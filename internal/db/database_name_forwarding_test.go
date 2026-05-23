package db

import (
	"context"
	"database/sql"
	"testing"
)

// The database-per-tenant strategies re-pin a connection to the engine's own
// database by name, and they get that name off the pool. The runtime does not
// hand them the pool it connected: it wraps it in a slow-query tracer first,
// and only then builds the router and the engine host. A decorator that does
// not forward the name turns the re-pin into a silent no-op.
//
// So the shape under test is the wrapped one, not the bare pool.
func TestDatabaseNameOf_SurvivesTheWrappersTheRuntimeBuilds(t *testing.T) {
	const name = "lyeve_engine"
	inner := &sqlDB{dbName: name}

	for _, tc := range []struct {
		shape string
		pool  DB
	}{
		{shape: "as connected", pool: inner},
		{shape: "wrapped in the slow-query tracer, as the runtime does", pool: NewSlowQueryTracer(inner, 50)},
		{shape: "wrapped twice", pool: NewSlowQueryTracer(NewSlowQueryTracer(inner, 50), 50)},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			if got := DatabaseNameOf(tc.pool); got != name {
				t.Errorf("database name %q, want %q: the tenancy strategy will not re-pin", got, name)
			}
		})
	}
}

// A pool that cannot report a name leaves connections alone rather than
// failing.
func TestDatabaseNameOf_UnreportedNameIsEmptyRatherThanFatal(t *testing.T) {
	if got := DatabaseNameOf(&namelessDB{}); got != "" {
		t.Errorf("database name %q, want empty", got)
	}
	if got := DatabaseNameOf(&sqlDB{}); got != "" {
		t.Errorf("database name %q, want empty", got)
	}
}

// namelessDB is a DB with no DatabaseName method, standing in for the test
// doubles across the repository that cannot answer it.
type namelessDB struct{}

func (*namelessDB) QueryRow(context.Context, string, ...any) (*sql.Row, error) { return nil, nil }
func (*namelessDB) Query(context.Context, string, ...any) (*sql.Rows, error)   { return nil, nil }
func (*namelessDB) Exec(context.Context, string, ...any) (sql.Result, error)   { return nil, nil }
func (*namelessDB) Begin(context.Context) (*sql.Tx, error)                     { return nil, nil }
func (*namelessDB) Conn(context.Context) (*sql.Conn, error)                    { return nil, nil }
func (*namelessDB) Ping(context.Context) error                                 { return nil }
func (*namelessDB) Close() error                                               { return nil }
func (*namelessDB) Stats() sql.DBStats                                         { return sql.DBStats{} }
func (*namelessDB) Engine() string                                             { return "mysql" }
func (*namelessDB) SQLDB() *sql.DB                                             { return nil }
func (*namelessDB) QuerierRO(context.Context) (ReadOnlyQuerier, error)         { return nil, nil }
