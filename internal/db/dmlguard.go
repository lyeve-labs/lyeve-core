// Package db DMLGuardConnector wraps a driver.Connector and intercepts Prepare/Exec to
// reject INSERT, UPDATE, DELETE, TRUNCATE, MERGE, and REPLACE. SELECT, DDL,
// and transaction control pass through. Mutations must go through
// host.Querier(ctx) or host.AdminQuerier(ctx).
//
// NewDMLGuardDB opens a tiny-pool (max 2 conns) *sql.DB with the guard wired
// into every connection. Use for PluginMigrate (DDL). Runtime queries use the
// primary pool via Querier/AdminQuerier.
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// errDMLRejected is returned when a DML statement is intercepted by the guard.
var errDMLRejected = errors.New("read-only database: DML statements (INSERT/UPDATE/DELETE/TRUNCATE/MERGE/REPLACE) are not allowed through RawDB - use host.Querier(ctx) for scoped queries or host.AdminQuerier(ctx) for admin operations")

// ErrDMLRejected is the exported form of errDMLRejected. Plugins and tests
// can use errors.Is(err, ErrDMLRejected) to detect DML rejection.
var ErrDMLRejected = errDMLRejected

// dmlPrefixRe matches SQL statements that begin with a DML verb, skipping
// leading whitespace, block comments (/* ... */), and line comments (-- to EOL).
// Case-insensitive. Covers INSERT, UPDATE, DELETE, TRUNCATE, MERGE, REPLACE.
var dmlPrefixRe = regexp.MustCompile(
	`(?i)^\s*(?:/\*.*?\*/\s*|--[^\n]*\n\s*)*(?:INSERT\b|UPDATE\b|DELETE\b|TRUNCATE\b|MERGE\b|REPLACE\b)`)

// IsDML reports whether the SQL statement starts with a DML verb after
// stripping leading whitespace and comments. Exported for test packages
// that verify DML-guarded connection behavior.
func IsDML(query string) bool {
	return dmlPrefixRe.MatchString(query)
}

// isDML is IsDML for callers inside this package.
func isDML(query string) bool { return IsDML(query) }

// DMLGuardConnector wraps a driver.Connector and returns guarded connections
// that reject DML statements. Use with sql.OpenDB to create a guarded *sql.DB.
type DMLGuardConnector struct {
	inner driver.Connector
}

// NewDMLGuardConnector wraps c so every connection it produces rejects DML.
func newDMLGuardConnector(c driver.Connector) *DMLGuardConnector {
	return &DMLGuardConnector{inner: c}
}

// Connect implements driver.Connector.
func (g *DMLGuardConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := g.inner.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("dml_guard: connect: %w", err)
	}
	return &dmlGuardConn{inner: conn}, nil
}

// Driver implements driver.Connector.
func (g *DMLGuardConnector) Driver() driver.Driver {
	return g.inner.Driver()
}

// dmlGuardConn wraps a driver.Conn and rejects DML on Prepare/PrepareContext,
// and on ExecContext (for drivers that support direct exec). All other calls
// forward to the inner connection.
type dmlGuardConn struct {
	inner driver.Conn
}

// Prepare is required by driver.Conn.
func (c *dmlGuardConn) Prepare(query string) (driver.Stmt, error) {
	if isDML(query) {
		return nil, errDMLRejected
	}
	return c.inner.Prepare(query)
}

// Close is required by driver.Conn.
func (c *dmlGuardConn) Close() error { return c.inner.Close() }

// Begin is required by driver.Conn.
func (c *dmlGuardConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

// Optional interfaces

// PrepareContext implements the optional driver.ConnPrepareContext.
func (c *dmlGuardConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if isDML(query) {
		return nil, errDMLRejected
	}
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
}

// ExecContext implements the optional driver.ExecerContext and guards DML.
func (c *dmlGuardConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if isDML(query) {
		return nil, errDMLRejected
	}
	if ec, ok := c.inner.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	// Fallback: Prepare then Stmt.Exec (DML already rejected above).
	stmt, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	values := make([]driver.Value, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	return stmt.Exec(values)
}

// QueryContext implements the optional driver.QueryerContext. It applies no
// DML filter (SELECT and DDL are allowed) but forwards for completeness.
func (c *dmlGuardConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if qc, ok := c.inner.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

// BeginTx implements the optional driver.ConnBeginTx.
func (c *dmlGuardConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.inner.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	return c.inner.Begin()
}

// Ping implements the optional driver.Pinger.
func (c *dmlGuardConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// ResetSession implements the optional driver.SessionResetter.
func (c *dmlGuardConn) ResetSession(ctx context.Context) error {
	if rs, ok := c.inner.(driver.SessionResetter); ok {
		return rs.ResetSession(ctx)
	}
	return nil
}

// Factory

// NewDMLGuardDB opens a new DML-guarded *sql.DB from a DSN and engine name.
// The returned pool is intentionally small (max 2 connections) because it
// serves only the read and DDL work plugins send through RawDB. Nothing may
// hold one of its connections for long: advisory-lock sessions come from
// NewLockDB instead.
func NewDMLGuardDB(ctx context.Context, engine string, dsn string) (*sql.DB, error) {
	return openGuarded(ctx, engine, dsn, 2, 1)
}

// NewLockDB opens the pool that advisory-lock sessions are drawn from. A
// leader keeps its lock, and so its session, for as long as it leads, so
// every held lock is a connection nobody else can use. From a capped pool a
// handful of leaders take every connection and the next lock waits for one
// forever. From the request pool they take capacity from requests. This pool
// is uncapped and holds one session per held lock plus those briefly trying.
// It carries the DML guard as well, because a lock needs no DML.
func NewLockDB(ctx context.Context, engine string, dsn string) (*sql.DB, error) {
	return openGuarded(ctx, engine, dsn, 0, 2)
}

func openGuarded(ctx context.Context, engine, dsn string, maxOpen, maxIdle int) (*sql.DB, error) {
	connector, err := connectorForDSN(engine, dsn)
	if err != nil {
		return nil, fmt.Errorf("dml_guard: create connector: %w", err)
	}
	guarded := sql.OpenDB(newDMLGuardConnector(connector))
	guarded.SetMaxOpenConns(maxOpen)
	guarded.SetMaxIdleConns(maxIdle)
	guarded.SetConnMaxLifetime(0)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := guarded.PingContext(pingCtx); err != nil {
		guarded.Close()
		return nil, fmt.Errorf("dml_guard: ping: %w", err)
	}
	return guarded, nil
}

// connectorForDSN creates a driver.Connector for the given engine+DSN.
func connectorForDSN(engine, dsn string) (driver.Connector, error) {
	driverName := driverNameFor(engine)
	switch engine {
	case "mssql":
		return mssql.NewConnector(dsn)
	default:
		// Open a temporary DB to extract the connector from the registered
		// driver (pgx or mysql). The temp DB is immediately discarded. Only
		// the connector is kept.
		tmpDB, err := sql.Open(driverName, dsn)
		if err != nil {
			return nil, fmt.Errorf("dml_guard: open temp db: %w", err)
		}
		defer tmpDB.Close()

		dc, ok := tmpDB.Driver().(driver.DriverContext)
		if !ok {
			return nil, fmt.Errorf("dml_guard: driver %q does not implement DriverContext", driverName)
		}
		return dc.OpenConnector(dsn)
	}
}
