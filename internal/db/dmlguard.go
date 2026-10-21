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
	"strings"
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

// IsDMLOn reports whether any statement in query starts with a DML verb, read
// the way engine's server reads it. A query can carry several statements: the
// MySQL guard pools allow them, PostgreSQL runs them on an Exec with no
// arguments, and SQL Server runs a batch. Reading only the first let
// "SELECT 1; DELETE FROM sys_users" through.
//
// A statement starts at the beginning of the query and after each semicolon
// that is not inside a string, a quoted identifier or a comment. Where the
// dialects differ on what quotes or comments, each engine's own rule is used,
// and text the server would run is never read as quoted. SQL Server can also
// run two statements with no semicolon between them, which this does not
// see.
func IsDMLOn(engine, query string) bool {
	for _, start := range statementStarts(engine, query) {
		if dmlPrefixRe.MatchString(query[start:]) {
			return true
		}
	}
	return false
}

// statementStarts returns the offset of each statement's first token in query.
func statementStarts(engine, q string) []int {
	var starts []int
	atStart := true
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case isSQLSpace(c):
			i++
			continue
		case c == '-' && strings.HasPrefix(q[i:], "--") && (engine != "mysql" || len(q) == i+2 || isSQLSpace(q[i+2])):
			// MySQL reads "--" as a comment only before whitespace.
			i = lineEnd(q, i)
			continue
		case c == '#' && engine == "mysql":
			i = lineEnd(q, i)
			continue
		case c == '/' && strings.HasPrefix(q[i:], "/*") && !(engine == "mysql" && strings.HasPrefix(q[i:], "/*!")):
			// MySQL runs the text of a /*! comment, so it is read as code.
			// A nested comment ends here at its first close, which only
			// reads more of the query as code.
			if end := strings.Index(q[i+2:], "*/"); end >= 0 {
				i += end + 4
			} else {
				i = len(q)
			}
			continue
		}
		if atStart {
			starts = append(starts, i)
			atStart = false
		}
		switch {
		case c == ';':
			atStart = true
			i++
		case c == '\'':
			i = quotedEnd(q, i, '\'', engine == "mysql" || isEscapeString(engine, q, i))
		case c == '"':
			i = quotedEnd(q, i, '"', engine == "mysql")
		case c == '`' && engine == "mysql":
			i = quotedEnd(q, i, '`', false)
		case c == '[' && engine == "mssql":
			i = quotedEnd(q, i, ']', false)
		case c == '$' && engine == "postgres" && (i == 0 || !isIdentByte(q[i-1])):
			i = dollarQuotedEnd(q, i)
		default:
			i++
		}
	}
	return starts
}

func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// lineEnd returns the offset after the line comment that starts at i.
func lineEnd(q string, i int) int {
	if nl := strings.IndexByte(q[i:], '\n'); nl >= 0 {
		return i + nl + 1
	}
	return len(q)
}

// isEscapeString reports whether the quote at i opens a PostgreSQL E'...'
// string, the one form in which a backslash escapes the quote.
func isEscapeString(engine, q string, i int) bool {
	if engine != "postgres" || i == 0 || (q[i-1] != 'E' && q[i-1] != 'e') {
		return false
	}
	return i == 1 || !isIdentByte(q[i-2])
}

// quotedEnd returns the offset after the quoted text that opens at i and
// closes at closer. A doubled closer stands for itself, and with backslash a
// backslash escapes the byte after it. An unclosed quote runs to the end.
func quotedEnd(q string, i int, closer byte, backslash bool) int {
	for j := i + 1; j < len(q); j++ {
		switch {
		case backslash && q[j] == '\\':
			j++
		case q[j] == closer:
			if j+1 < len(q) && q[j+1] == closer {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(q)
}

// dollarQuotedEnd returns the offset after the PostgreSQL dollar-quoted string
// that opens at i, or i+1 when the dollar opens none, as in a $1 parameter.
func dollarQuotedEnd(q string, i int) int {
	j := i + 1
	for j < len(q) && q[j] != '$' && isIdentByte(q[j]) && !(j == i+1 && '0' <= q[j] && q[j] <= '9') {
		j++
	}
	if j >= len(q) || q[j] != '$' {
		return i + 1
	}
	tag := q[i : j+1]
	if end := strings.Index(q[j+1:], tag); end >= 0 {
		return j + 1 + end + len(tag)
	}
	return len(q)
}

// DMLGuardConnector wraps a driver.Connector and returns guarded connections
// that reject DML statements. Use with sql.OpenDB to create a guarded *sql.DB.
type DMLGuardConnector struct {
	inner  driver.Connector
	engine string
}

// NewDMLGuardConnector wraps c so every connection it produces rejects DML.
func newDMLGuardConnector(c driver.Connector, engine string) *DMLGuardConnector {
	return &DMLGuardConnector{inner: c, engine: engine}
}

// Connect implements driver.Connector.
func (g *DMLGuardConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := g.inner.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("dml_guard: connect: %w", err)
	}
	return &dmlGuardConn{inner: conn, engine: g.engine}, nil
}

// Driver implements driver.Connector.
func (g *DMLGuardConnector) Driver() driver.Driver {
	return g.inner.Driver()
}

// dmlGuardConn wraps a driver.Conn and rejects DML on Prepare/PrepareContext,
// and on ExecContext (for drivers that support direct exec). All other calls
// forward to the inner connection.
type dmlGuardConn struct {
	inner  driver.Conn
	engine string
}

// Prepare is required by driver.Conn.
func (c *dmlGuardConn) Prepare(query string) (driver.Stmt, error) {
	if IsDMLOn(c.engine, query) {
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
	if IsDMLOn(c.engine, query) {
		return nil, errDMLRejected
	}
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
}

// ExecContext implements the optional driver.ExecerContext and guards DML.
func (c *dmlGuardConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if IsDMLOn(c.engine, query) {
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
	connector, err := connectorForDSN(engine, driverDSN(engine, dsn))
	if err != nil {
		return nil, fmt.Errorf("dml_guard: create connector: %w", err)
	}
	guarded := sql.OpenDB(newDMLGuardConnector(connector, engine))
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
