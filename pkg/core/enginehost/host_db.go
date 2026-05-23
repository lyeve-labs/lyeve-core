package enginehost

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Pool exposes the underlying connection pool to engine code in the same
// binary. Its type lives in internal/db, so a plugin cannot reach it: Go
// refuses an internal import across a module boundary.
func (h *engineHost) Pool() db.DB { return h.pool }

// RawDB implements core.Host.RawDB. Returns the DML-guarded DB when a
// guard has been wired (via WithDMLGuardedDB), or the raw *sql.DB
// when no guard is wired. Plugins should never reach for
// RawDB for DML: use host.Querier(ctx) for scoped queries, or
// type-assert to AdminQuerierProvider for admin DML.
func (h *engineHost) RawDB() *sql.DB {
	if h.guardedDB != nil {
		return h.guardedDB
	}
	return h.rawDB
}

// MigrationDB implements core.Host.MigrationDB. Always returns the real
// unscoped *sql.DB so PluginMigrate works without restriction.
//
// The DML guard on RawDB deliberately does NOT apply here, and must not be
// added. A migration script runs seed-data DML against the tables it created,
// and PluginMigrate itself writes its own bookkeeping table. A guard, whether
// blanket or scoped to a list of tables, would break both.
//
// Access is gated by CapRawDB (see core.ScopedHost.MigrationDB): a plugin
// trusted to run migrations is, by design, trusted to run migration-time DML.
// That capability is the isolation boundary, not statement filtering.
func (h *engineHost) MigrationDB() *sql.DB { return h.rawDB }

// WithDMLGuardedDB wires a DML-guarded *sql.DB to be served by RawDB().
// The guarded DB rejects INSERT/UPDATE/DELETE/TRUNCATE/MERGE/REPLACE
// while allowing SELECT and DDL (PluginMigrate). The real pool is
// still served by MigrationDB() and AdminQuerier().
func (h *engineHost) WithDMLGuardedDB(db *sql.DB) { h.guardedDB = db }

// WithLockDB wires the pool every DistLock this host issues holds its
// session on. Plugins pass RawDB to TryAcquire, and a leader keeps its
// session while it leads, so without this pool a few leaders would take
// every connection RawDB has.
func (h *engineHost) WithLockDB(db *sql.DB) { h.lockDB = db }

// AdminQuerier implements AdminQuerierProvider. Returns a core.Querier that
// talks directly to the engine's raw *sql.DB pool: no tenant isolation,
// no DML guard. Plugins that use this MUST gate behind RequireSuperAdmin
// or equivalent auth.
func (h *engineHost) AdminQuerier(ctx context.Context) core.Querier {
	return &adminDBQuerier{db: h.rawDB}
}

// AcquireTenantConn implements TenancyConnProvider for tenant-scoped gRPC
// connections. Mirrors the HTTP TenancyConn middleware: acquires a dedicated
// *sql.Conn, applies the dialect-appropriate isolation strategy (PG: SET
// search_path, MySQL/MSSQL: USE database), and stows it on the returned
// context. The caller MUST defer cleanup so the conn is reset+returned to
// the pool even when the handler panics or errors.
//
// In single-tenant mode (cfg.MultiTenant == false) or when tenantID is
// empty, this is a transparent pass-through: returns (ctx, noop, nil)
// with no connection acquisition.
func (h *engineHost) AcquireTenantConn(ctx context.Context, tenantID string) (context.Context, func(), error) {
	noop := func() {}
	if !h.cfg.MultiTenant || tenantID == "" {
		return ctx, noop, nil
	}

	// Every tenancy strategy resolves the tenant by calling
	// core.TenantIDFromCtx, so the argument this function was given and the
	// scope it ends up applying are two separate values that nothing forces
	// to agree. Stamping the argument makes them the same value by
	// construction, and makes the returned context agree with the connection
	// it carries.
	ctx = core.WithTenantID(ctx, tenantID)

	// Acquire a fresh *sql.Conn from the pool.
	conn, err := h.pool.Conn(ctx)
	if err != nil {
		return ctx, noop, fmt.Errorf("acquire conn: %w", err)
	}

	tenancy := h.tenancy()
	if tenancy == nil {
		// No tenancy strategy configured: pass through.
		_ = conn.Close()
		return ctx, noop, nil
	}

	if err := tenancy.Apply(ctx, conn); err != nil {
		// Apply can fail partway (the USE succeeded and a later statement
		// did not), so what state the session is in is unknown. Close would
		// hand it back to the pool in that state, which is why it is
		// discarded instead.
		discardConn(conn)
		return ctx, noop, fmt.Errorf("apply tenant isolation: %w", err)
	}

	cleanup := func() {
		// Reset on a fresh bounded context so cleanup runs even when the
		// request context was canceled mid-handler.
		resetCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := tenancy.Reset(resetCtx, conn); err != nil {
			// Close does not close anything: it returns the connection to the
			// pool with its session state intact. A reset that failed leaves
			// search_path (or the selected database) still naming this tenant,
			// so the next caller to borrow it reads that tenant's rows under
			// its own credential and nothing notices.
			slog.ErrorContext(resetCtx, "tenant isolation reset failed - discarding the connection rather than returning it to the pool",
				"err", err, "tenant", tenantID)
			discardConn(conn)
		}
		_ = conn.Close() // best-effort
	}

	return db.WithTenantConn(ctx, conn), cleanup, nil
}

// tenancy lazily creates the dialect-appropriate Tenancy strategy from the
// pool's engine. Returns nil when unreachable (single-tenant path should
// gate via cfg.MultiTenant first).
func (h *engineHost) tenancy() db.Tenancy {
	h.tenancyOnce.Do(func() {
		tidFn := core.TenantIDFromCtx
		switch h.pool.Engine() {
		case "mysql":
			h.tenancyVal = db.NewMySQLDatabaseTenancy(tidFn, db.DatabaseNameOf(h.pool))
		case "mssql":
			h.tenancyVal = db.NewMSSQLDatabaseTenancy(tidFn, db.DatabaseNameOf(h.pool))
		default:
			h.tenancyVal = db.NewPostgresSchemaTenancy(tidFn)
		}
	})
	return h.tenancyVal
}

type poolQuerier struct{ pool db.DB }

func (q *poolQuerier) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	// pgx.core.Row already satisfies the core.Row interface (Scan(dest ...any) error).
	row, err := q.pool.QueryRow(ctx, sql, args...)
	if err != nil {
		return core.ErrorRow(err), err
	}
	return row, nil
}

func (q *poolQuerier) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	rows, err := q.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

func (q *poolQuerier) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	tag, err := q.pool.Exec(ctx, sql, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := tag.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}

func (q *poolQuerier) Begin(ctx context.Context) (core.Tx, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Carry the engine and the engine's own database so statements inside the
	// transaction are rewritten the same way the pool rewrites the ones outside
	// it: placeholders, and sys_* qualification for a transaction that inherited
	// a tenant-scoped connection.
	return &pgxTx{tx: tx, engine: q.pool.Engine(), sysDB: poolDatabaseName(q.pool)}, nil
}

// BeginIsolated implements core.IsolatedTxBeginner. A pool that cannot honor
// the level starts an ordinary transaction rather than failing the caller:
// the levels this asks for are a contention concern, not a correctness one.
func (q *poolQuerier) BeginIsolated(ctx context.Context, level core.IsolationLevel) (core.Tx, error) {
	isolated, ok := q.pool.(interface {
		BeginIsolated(context.Context, sql.IsolationLevel) (*sql.Tx, error)
	})
	if !ok || level == core.IsolationDefault {
		return q.Begin(ctx)
	}
	tx, err := isolated.BeginIsolated(ctx, sqlIsolation(level))
	if err != nil {
		return nil, err
	}
	return &pgxTx{tx: tx, engine: q.pool.Engine(), sysDB: poolDatabaseName(q.pool)}, nil
}

// sqlIsolation maps the levels core names to database/sql's.
func sqlIsolation(level core.IsolationLevel) sql.IsolationLevel {
	if level == core.IsolationReadCommitted {
		return sql.LevelReadCommitted
	}
	return sql.LevelDefault
}

// poolDatabaseName reports the pool's own database, or "" when the pool cannot
// answer. Unlike db.DatabaseNameOf it says nothing when the answer is missing:
// that diagnostic belongs to tenancy setup, which runs once at boot, not to a
// path that runs on every transaction.
func poolDatabaseName(pool db.DB) string {
	namer, ok := pool.(interface{ DatabaseName() string })
	if !ok {
		return ""
	}
	return namer.DatabaseName()
}

// roQuerier: wraps db.ReadOnlyQuerier to implement core.Querier (Exec/Begin error)

type roQuerier struct{ ro db.ReadOnlyQuerier }

func (q *roQuerier) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	row, err := q.ro.QueryRow(ctx, sql, args...)
	if err != nil {
		return core.ErrorRow(err), err
	}
	return row, nil
}

func (q *roQuerier) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	rows, err := q.ro.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

// Exec is not supported on read-only queriers: demand a primary querier for writes.
func (q *roQuerier) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	return core.CommandTag{}, errors.New("read-only querier: Exec not allowed")
}

// Begin is not supported on read-only queriers: demand a primary querier for writes.
func (q *roQuerier) Begin(ctx context.Context) (core.Tx, error) {
	return nil, errors.New("read-only querier: Begin not allowed")
}

// adminDBQuerier wraps *sql.DB directly for AdminQuerier, with no
// tenant isolation, no DML guard, and no query rewriting. It is intended for
// operational and admin queries gated by RequireSuperAdmin.
type adminDBQuerier struct{ db *sql.DB }

func (q *adminDBQuerier) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	return q.db.QueryRowContext(ctx, sql, args...), nil
}

func (q *adminDBQuerier) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	rows, err := q.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

func (q *adminDBQuerier) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	res, err := q.db.ExecContext(ctx, sql, args...)
	if err != nil {
		return core.CommandTag{}, err
	}
	n, _ := res.RowsAffected()
	return core.CommandTag{RowsAffected: n}, nil
}

func (q *adminDBQuerier) Begin(ctx context.Context) (core.Tx, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// No engine, so no rewriting: the same contract as this querier's own
	// methods, which hand SQL to the raw pool in the engine's own dialect.
	return &pgxTx{tx: tx}, nil
}

// pgxRows: wraps *sql.Rows to implement core.Rows

type pgxRows struct{ rows *sql.Rows }

func (r *pgxRows) Next() bool             { return r.rows.Next() }
func (r *pgxRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *pgxRows) Close()                 { _ = r.rows.Close() }
func (r *pgxRows) Err() error             { return r.rows.Err() }

// discardConn keeps a connection out of the pool. database/sql closes a
// connection whose Raw callback returns ErrBadConn rather than reusing it,
// which is the only way to stop Close from handing back a session whose
// isolation is unknown or could not be undone. Losing one connection is the
// cheap outcome.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

// EngineDBConn implements core.EngineDBConnProvider. It takes a connection
// from the engine's own pool and binds it back to the engine's database, so a
// caller issuing unqualified DDL writes where it means to.
//
// The pool is shared with the request path, which binds connections to tenant
// databases on MySQL and SQL Server, and a pooled connection keeps that
// binding when it is returned. Without the re-bind a plugin's DDL lands in
// whichever tenant last used the connection, or fails with "Unknown database"
// once that tenant is deleted. The engine's own schema work does this for
// itself. This is the same guarantee, offered to a plugin that cannot
// reach the database name to do it.
func (h *engineHost) EngineDBConn(ctx context.Context) (*sql.Conn, error) {
	if h.rawDB == nil {
		return nil, errors.New("engine database connection: no pool on this host")
	}
	conn, err := h.rawDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine database connection: %w", err)
	}
	if err := db.PinConnToEngineDB(ctx, h.pool, conn); err != nil {
		// A failed re-bind leaves the session on whatever database it was
		// already on, which is the tenant database this call exists to get
		// off. Close would hand that connection back to the pool in exactly
		// the state the success path prevents, so it is discarded instead.
		discardConn(conn)
		return nil, fmt.Errorf("engine database connection: %w", err)
	}
	return conn, nil
}

var _ core.WriteSerializer = (*engineHost)(nil)

// SerializeWrite implements core.WriteSerializer over the engine's pool. A
// host with no database has nothing to serialize against and runs fn as it
// is.
func (h *engineHost) SerializeWrite(ctx context.Context, key string, fn func(context.Context) error) error {
	if h.pool == nil {
		return fn(ctx)
	}
	return db.SerializeWrite(ctx, h.pool, key, fn)
}
