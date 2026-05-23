// Package db provides the multi-dialect database abstraction for LyEve CMS.
// It wraps PostgreSQL, MySQL, and MSSQL connections behind a unified Querier
// interface with automatic placeholder rewriting ($N -> ? or @pN), connection
// pooling, prepared statement caching, and per-engine SQL dialect support.
package db

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	mssql "github.com/microsoft/go-mssqldb"

	_ "github.com/go-sql-driver/mysql"                      // registers the "mysql" driver name
	_ "github.com/golang-migrate/migrate/v4/database/mysql" // registers "mysql" migrate driver
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/database/sqlserver"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver name on sql.Open
	// "sqlserver" driver registered in connect.go
)

// safeSlugRe restricts tenant slugs to lowercase alphanumeric + underscore,
// 1-63 chars. Used by the Tenancy strategy to keep search_path
// SQL injection-proof.
var safeSlugRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// safeDatabaseNameRe restricts the engine's own database name, which the
// Tenancy strategies interpolate into a USE statement. Wider than safeSlugRe
// because that name is chosen by whoever deployed the engine and is under no
// obligation to look like a tenant slug: mixed case and hyphens are both legal
// and both common. Still narrow enough that nothing can escape the backtick or
// bracket quoting the name goes through.
var safeDatabaseNameRe = regexp.MustCompile(`^[A-Za-z0-9_$-]{1,64}$`)

// ConnectOptions carries optional pool tuning parameters. Tenant isolation
// is not a pool-acquire hook. It is a sql.Conn-based middleware that
// wraps each request (see Tenancy in tenancy.go and TenancyConn in
// internal/middleware).
type ConnectOptions struct {
	MaxConns        int32
	MinConns        int32
	ConnMaxLifetime time.Duration // max lifetime before recycling (default 1h)
	ConnMaxIdleTime time.Duration // max idle time before closing (default 5m)
	// Deprecated: use ConnMaxIdleTime instead.
	HealthCheckPeriod time.Duration
	StatementTimeout  time.Duration // max query execution time (default 30s), set via DSN on PG/MySQL and via LOCK_TIMEOUT on MSSQL
	ConnectTimeout    time.Duration // max time to establish a connection (default 10s, all engines)
}

// DB is the seam every part of the engine takes instead of a concrete connection
// pool. Backed by *sql.DB via pgx/v5/stdlib. The same interface also hosts MySQL
// (go-sql-driver/mysql) and MSSQL (microsoft/go-mssqldb). Method names mirror
// the standard database/sql package so call sites read the same across engines.
//
// Placeholder rewrites ($N -> ? for MySQL, -> @pN for MSSQL) happen inside the
// impl so call-site SQL stays in Postgres style. Postgres passes through
// untouched.
//
// When ctx carries a tenant-isolated *sql.Conn (set by the TenancyConn
// middleware), QueryRow / Query / Exec / Begin route through that conn so the
// SET search_path applied at request start stays in effect for every query in
// the request: without changing a single store call site.
type DB interface {
	// QueryRow executes a query that returns at most one row.
	QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error)

	// Query executes a query that returns multiple rows.
	Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error)

	// Exec executes a query without returning rows (INSERT, UPDATE, DELETE).
	Exec(ctx context.Context, sql string, args ...any) (sql.Result, error)

	// Begin starts a new transaction.
	Begin(ctx context.Context) (*sql.Tx, error)

	// Conn returns a dedicated connection from the pool.
	Conn(ctx context.Context) (*sql.Conn, error)

	// Ping verifies the database connection is still alive.
	Ping(ctx context.Context) error

	// Close closes the connection pool and releases all resources.
	Close() error

	// Stats returns pool statistics for diagnostics.
	Stats() sql.DBStats

	// Engine returns the database engine name, e.g. "postgres", "mysql", "mssql".
	// Plugins use this to select dialect-specific migration files and DDL.
	Engine() string

	// SQLDB returns the underlying *sql.DB. Plugins use this for migration
	// runners and other database operations that require the raw *sql.DB.
	SQLDB() *sql.DB

	// QuerierRO returns a read-only querier. When a replica pool is configured
	// the returned Querier reads from the replica. Otherwise it falls back to
	// the primary. Dashboards, schema listings, and other read-only handlers
	// should prefer this method to reduce load on the primary.
	QuerierRO(ctx context.Context) (ReadOnlyQuerier, error)
}

// ReadOnlyQuerier is the subset of DB used by read-only handlers (dashboard,
// schema listing). It exposes QueryRow and Query: no Exec, Begin, or writes.
type ReadOnlyQuerier interface {
	// QueryRow executes a read-only query that returns at most one row.
	QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error)

	// Query executes a read-only query that returns multiple rows.
	Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error)
}

// RewritePlaceholders converts Postgres-style $N placeholders to the
// engine-appropriate form, and for MySQL reorders args to match the positional
// ? order. Postgres returns the input unchanged. MySQL maps every $N to a
// positional `?` and reorders args so that arg[N-1] is bound to the ? that
// replaced $N (regardless of N's position in the SQL text). MSSQL maps $N to
// the named parameter `@pN`: args pass through unchanged because go-mssqldb
// handles @pN->arg binding by name.
//
// Dollar tokens are skipped inside three contexts so they survive verbatim:
//
//   - Single-quoted string literals (including the SQL doubled-single-quote
//     escape for an embedded quote) - `'$1 is a price'` stays literal.
//   - SQL line comments (`-- to end of line`).
//   - SQL block comments (`/* ... */`, non-nested - matches MySQL semantics).
//
// Postgres dollar-quoted strings ($tag$...$tag$) are NOT handled because they
// never reach this code path: they're a Postgres-only feature, used in
// queries that run on Postgres directly (passthrough).
// RewritePlaceholders exposes the rewriter to the parts of the engine that hold
// a raw *sql.Tx rather than a pool: the transaction adapters in
// pkg/core/enginehost. Everything a plugin issues outside a transaction is
// rewritten by sqlDB. A plugin should not have to write different SQL because
// its statement happens to be inside one. The plugin test harness in
// pkg/plugintest and this package's benchmarks call it too.
func RewritePlaceholders(q, engine string, args []any) (string, []any) {
	return rewritePlaceholders(q, engine, args)
}

func rewritePlaceholders(q, engine string, args []any) (string, []any) {
	if engine == "postgres" {
		return q, args
	}
	var b strings.Builder
	// order collects $N numbers in the order they appear in the rewritten SQL.
	// MySQL's go-sql-driver binds ? positionally, so $2 appearing first in the
	// source SQL means the first ? must receive args[1], not args[0].
	// MSSQL does not need this, because @pN is named.
	var order []int
	inStr := false
	i := 0
	for i < len(q) {
		c := q[i]
		// Line comment: - to end of line. Only outside strings.
		if !inStr && c == '-' && i+1 < len(q) && q[i+1] == '-' {
			j := i
			for j < len(q) && q[j] != '\n' {
				j++
			}
			b.WriteString(q[i:j])
			i = j
			continue
		}
		// Block comment: /* ... */. Only outside strings. The rewriter treats the FIRST
		// */ as the close (non-nested), matching MySQL/MSSQL semantics: the
		// only engines this code path actually runs on after rewrite.
		if !inStr && c == '/' && i+1 < len(q) && q[i+1] == '*' {
			j := i + 2
			for j+1 < len(q) && (q[j] != '*' || q[j+1] != '/') {
				j++
			}
			end := j + 2
			if j+1 >= len(q) {
				end = len(q)
				// unterminated block: copy to end of input
			}
			b.WriteString(q[i:end])
			i = end
			continue
		}
		if c == '\'' {
			// SQL escape for ' inside a string is ''. Consume both so the parser does not
			// flip out of the string mid-escape.
			if inStr && i+1 < len(q) && q[i+1] == '\'' {
				b.WriteByte('\'')
				b.WriteByte('\'')
				i += 2
				continue
			}
			inStr = !inStr
			b.WriteByte(c)
			i++
			continue
		}
		if !inStr && c == '$' && i+1 < len(q) && isASCIIDigit(q[i+1]) {
			j := i + 1
			for j < len(q) && isASCIIDigit(q[j]) {
				j++
			}
			n := 0
			for k := i + 1; k < j; k++ {
				n = n*10 + int(q[k]-'0')
			}
			switch engine {
			case "mysql":
				order = append(order, n)
				b.WriteByte('?')
			case "mssql":
				b.WriteString("@p")
				b.WriteString(q[i+1 : j])
			default:
				// Unknown engine: leave the token alone rather than silently
				// rewriting incorrectly.
				b.WriteString(q[i:j])
			}
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}

	// MySQL: reorder args so positional ? bindings match $N semantics.
	if engine == "mysql" && len(order) > 0 {
		reordered := make([]any, len(order))
		for i, n := range order {
			if n < 1 || n > len(args) {
				return b.String(), args // bogus $N: leave args as-is
			}
			reordered[i] = args[n-1]
		}
		return b.String(), reordered
	}

	if engine == "mssql" {
		return b.String(), narrowUUIDArgs(args)
	}

	return b.String(), args
}

// narrowUUIDArgs binds uuid.UUID values as non-Unicode text on SQL Server.
//
// uuid.UUID is a driver.Valuer that yields a string, and go-mssqldb sends every
// Go string as NVARCHAR. NVARCHAR outranks CHAR and VARCHAR in SQL Server's
// data-type precedence, so comparing one against a CHAR(36) key column converts
// THE COLUMN rather than the parameter. That is non-sargable: the index is
// still there and can no longer be seeked. A primary-key lookup becomes a
// clustered index scan, and a MERGE holding a range lock over the rows its plan
// touches locks an entire table to upsert one row.
//
// Narrowing is safe here in a way it would not be for free text: a UUID's
// textual form is ASCII by construction, so no code page can lose information.
// Never do this to ordinary string arguments, which would silently mangle every
// character outside the collation's code page.
//
// A UNIQUEIDENTIFIER column is unaffected: the varchar value converts on the
// parameter side, which leaves the seek intact.
func narrowUUIDArgs(args []any) []any {
	var out []any
	for i, a := range args {
		var narrowed any
		switch v := a.(type) {
		case uuid.UUID:
			narrowed = mssql.VarChar(v.String())
		case *uuid.UUID:
			if v != nil {
				narrowed = mssql.VarChar(v.String())
			}
		case uuid.NullUUID:
			if v.Valid {
				narrowed = mssql.VarChar(v.UUID.String())
			}
		}
		if narrowed == nil {
			continue
		}
		if out == nil {
			out = make([]any, len(args))
			copy(out, args)
		}
		out[i] = narrowed
	}
	if out == nil {
		return args
	}
	return out
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }
