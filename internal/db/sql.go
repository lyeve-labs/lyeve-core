package db

import (
	"context"
	"database/sql"
	"fmt"

	"go.opentelemetry.io/otel/trace"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/tracing"
)

// sqlDB is the *sql.DB-backed implementation of DB. The dialect field drives
// the placeholder rewrite. For Postgres it is a no-op passthrough.
// When dbTracer is non-nil, QueryRow/Query/Exec/Begin are wrapped with
// OpenTelemetry spans.
//
// replica is an optional read-only connection pool (for QuerierRO). When nil,
// QuerierRO delegates to the primary pool. When set, read-only queries route
// to the replica to reduce load on the primary.
type sqlDB struct {
	db       *sql.DB
	dialect  dialect.Dialect
	dbTracer *tracing.DBTracer
	replica  *sql.DB // optional read-only replica pool
	dbName   string  // database the DSN selected, or "" when the server did not report one
}

// SetDBTracer injects the OpenTelemetry DB tracer after construction.
// Called by the runtime after tracing initialization.
func (s *sqlDB) SetDBTracer(t *tracing.DBTracer) { s.dbTracer = t }

// Engine returns the dialect name this pool is connected to.
func (s *sqlDB) Engine() string { return s.dialect.Name() }

// DatabaseName returns the database this pool's DSN selected, as the server
// itself reports it. Empty for Postgres, which has no use for it: its tenancy
// strategy switches search_path rather than databases.
func (s *sqlDB) DatabaseName() string { return s.dbName }

// rewrite translates Postgres-style $N placeholders to the form the target
// engine expects and, for MySQL, returns the reordered args so positional ?
// maps correctly. Postgres is a passthrough. MSSQL is a passthrough for args
// because @pN named parameters are handled by go-mssqldb.
//
// sys_* table references are qualified with the engine's own database first, so
// they still resolve on a connection the tenancy strategy has switched to a
// tenant database. Qualification runs before the placeholder pass because it
// works on Postgres-style SQL, and the database name it inserts contains no
// placeholder token for the second pass to trip over.
func (s *sqlDB) rewrite(q string, args []any) (string, []any) {
	if s.dialect == nil {
		return q, args
	}
	engine := s.dialect.Name()
	q = QualifySysTables(q, engine, s.dbName)
	return rewritePlaceholders(q, engine, args)
}

// traceQuery opens a DB span when tracing is enabled, otherwise returns a noop span.
func (s *sqlDB) traceQuery(ctx context.Context, operation, sql string) (context.Context, trace.Span) {
	if s.dbTracer != nil {
		return s.dbTracer.Span(ctx, operation, tracing.SQLSummary(sql))
	}
	return ctx, trace.SpanFromContext(ctx)
}

func (s *sqlDB) QueryRow(ctx context.Context, q string, args ...any) (*sql.Row, error) {
	ctx, span := s.traceQuery(ctx, "queryrow", q)
	defer span.End()
	q, args = s.rewrite(q, args)
	if tx := writeTx(ctx); tx != nil {
		return tx.QueryRowContext(ctx, q, args...), nil
	}
	conn, err := tenantConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant conn: %w", err)
	}
	if conn != nil {
		return conn.QueryRowContext(ctx, q, args...), nil
	}
	return s.db.QueryRowContext(ctx, q, args...), nil
}

func (s *sqlDB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	ctx, span := s.traceQuery(ctx, "query", q)
	defer span.End()
	q, args = s.rewrite(q, args)
	if tx := writeTx(ctx); tx != nil {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			tracing.RecordError(span, err)
		}
		return rows, err
	}
	conn, err := tenantConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant conn: %w", err)
	}
	if conn != nil {
		rows, err := conn.QueryContext(ctx, q, args...)
		if err != nil {
			tracing.RecordError(span, err)
		}
		return rows, err
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		tracing.RecordError(span, err)
	}
	return rows, err
}

func (s *sqlDB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	ctx, span := s.traceQuery(ctx, "exec", q)
	defer span.End()
	q, args = s.rewrite(q, args)
	if tx := writeTx(ctx); tx != nil {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			tracing.RecordError(span, err)
		}
		return res, err
	}
	conn, err := tenantConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant conn: %w", err)
	}
	if conn != nil {
		res, err := conn.ExecContext(ctx, q, args...)
		if err != nil {
			tracing.RecordError(span, err)
		}
		return res, err
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		tracing.RecordError(span, err)
	}
	return res, err
}

func (s *sqlDB) Begin(ctx context.Context) (*sql.Tx, error) {
	ctx, span := s.traceQuery(ctx, "begin", "BEGIN")
	defer span.End()
	if writeTx(ctx) != nil {
		return nil, ErrNestedWriteLock
	}
	conn, err := tenantConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant conn: %w", err)
	}
	if conn != nil {
		return conn.BeginTx(ctx, nil)
	}
	return s.db.BeginTx(ctx, nil)
}

// BeginIsolated starts a transaction at the given isolation level, on the
// request's tenant connection when there is one.
//
// The caller that needs this is the tenant purge. Its handlers delete by
// tenant_id across dozens of tables, and under MySQL's default REPEATABLE READ
// a range delete on a secondary index gap-locks the gaps it scans. Deleting one
// tenant's rows merges the gap so it spans where a neighboring tenant's insert
// would go, and the purge holds that for the whole transaction: an unrelated
// tenant's write blocks behind it, and deadlocks with it when the two take the
// same tables in opposite orders, which they do because foreign keys force the
// purge child-to-parent while a write goes parent-to-child.
//
// READ COMMITTED takes no gap locks for a range scan. The purge deletes rows for
// exactly one tenant and that tenant is being removed in the same transaction,
// so keeping other tenants' inserts out buys it nothing.
//
// Postgres and SQL Server already default to READ COMMITTED, so this is a no-op
// there.
func (s *sqlDB) BeginIsolated(ctx context.Context, level sql.IsolationLevel) (*sql.Tx, error) {
	ctx, span := s.traceQuery(ctx, "begin", "BEGIN")
	defer span.End()
	opts := &sql.TxOptions{Isolation: level}
	if writeTx(ctx) != nil {
		return nil, ErrNestedWriteLock
	}
	conn, err := tenantConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant conn: %w", err)
	}
	if conn != nil {
		return conn.BeginTx(ctx, opts)
	}
	return s.db.BeginTx(ctx, opts)
}

func (s *sqlDB) Conn(ctx context.Context) (*sql.Conn, error) { return s.db.Conn(ctx) }

func (s *sqlDB) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *sqlDB) Close() error                   { return s.db.Close() }

// SQLDB returns the underlying *sql.DB for external plugin access.
func (s *sqlDB) SQLDB() *sql.DB { return s.db }

func (s *sqlDB) Stats() sql.DBStats { return s.db.Stats() }

// QuerierRO returns a read-only querier. When a replica pool is configured
// (via ConnectReplica), reads go through the replica. Otherwise, falls back
// to the primary pool: backward-compatible no-op.
func (s *sqlDB) QuerierRO(ctx context.Context) (ReadOnlyQuerier, error) {
	rp := s.replica
	if rp == nil {
		rp = s.db
	}
	if err := rp.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("read replica ping: %w", err)
	}
	return &readOnlyQuerier{db: rp, dialect: s.dialect, dbTracer: s.dbTracer, dbName: s.dbName}, nil
}

// readOnlyQuerier backs QuerierRO: QueryRow + Query only.
type readOnlyQuerier struct {
	db       *sql.DB
	dialect  dialect.Dialect
	dbTracer *tracing.DBTracer
	dbName   string // engine's own database, for qualifying sys_* references
}

// rewrite applies the same sys_* qualification and placeholder translation the
// primary pool applies. A replica serves the same schema as the primary, so a
// read-only statement needs the sys_* rewrite for the same reason a write does.
func (r *readOnlyQuerier) rewrite(q string, args []any) (string, []any) {
	engine := r.dialect.Name()
	q = QualifySysTables(q, engine, r.dbName)
	return rewritePlaceholders(q, engine, args)
}

func (r *readOnlyQuerier) traceQuery(ctx context.Context, operation, sql string) (context.Context, trace.Span) {
	if r.dbTracer != nil {
		return r.dbTracer.Span(ctx, operation, tracing.SQLSummary(sql))
	}
	return ctx, trace.SpanFromContext(ctx)
}

func (r *readOnlyQuerier) QueryRow(ctx context.Context, q string, args ...any) (*sql.Row, error) {
	ctx, span := r.traceQuery(ctx, "queryrow", q)
	defer span.End()
	q, args = r.rewrite(q, args)
	conn, err := tenantConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant conn: %w", err)
	}
	if conn != nil {
		return conn.QueryRowContext(ctx, q, args...), nil
	}
	return r.db.QueryRowContext(ctx, q, args...), nil
}

func (r *readOnlyQuerier) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	ctx, span := r.traceQuery(ctx, "query", q)
	defer span.End()
	q, args = r.rewrite(q, args)
	conn, err := tenantConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant conn: %w", err)
	}
	if conn != nil {
		rows, err := conn.QueryContext(ctx, q, args...)
		if err != nil {
			tracing.RecordError(span, err)
		}
		return rows, err
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		tracing.RecordError(span, err)
	}
	return rows, err
}

// tenantConn returns the tenant-scoped *sql.Conn for this request, if any.
func tenantConn(ctx context.Context) (*sql.Conn, error) {
	if conn := TenantConn(ctx); conn != nil {
		return conn, nil
	}
	lc := LazyTenantConnFromCtx(ctx)
	if lc == nil {
		return nil, nil // no tenant scope: use pool directly
	}
	return lc.Acquire(ctx)
}
