package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/go-sql-driver/mysql"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
	mssql "github.com/microsoft/go-mssqldb"
)

// Tenancy isolates a per-request *sql.Conn to a single tenant.
// PostgresSchemaTenancy is the Postgres path (schema-per-tenant via
// search_path). MySQLDatabaseTenancy and MSSQLDatabaseTenancy use
// database-per-tenant (USE tenant_<slug>).
//
// With *sql.DB there is no per-acquire hook, so middleware acquires a
// *sql.Conn once per request, calls Apply, stows it in ctx via WithTenantConn,
// and queries through the DB wrapper transparently use that conn (see
// sqlDB.QueryRow et al.).
type Tenancy interface {
	// Apply isolates conn to the tenant identified by ctx, and where no tenant
	// is identified puts conn back on the engine's own scope. Either way conn
	// is in a known scope afterwards, whatever the previous request left on it.
	// Returns an error only when the tenant value is malformed or the isolation
	// SQL fails.
	Apply(ctx context.Context, conn *sql.Conn) error

	// Reset clears tenant state on conn before returning it to the pool.
	// Belt-and-braces: callers treat its error as a warning, so it cannot be
	// what guarantees the next request a known scope. Apply is.
	Reset(ctx context.Context, conn *sql.Conn) error
}

// PostgresSchemaTenancy implements schema-per-tenant isolation via
// `SET search_path = tenant_<slug>, public`. The slug is resolved from ctx by
// TenantIDFunc: typically the package-public TenantIDFromContext shipped by
// internal/middleware, so JWT-claim-derived slugs flow in unchanged.
type PostgresSchemaTenancy struct {
	// TenantIDFunc returns the tenant slug for the current request, or "" for
	// no isolation. Must validate that the value came from a trusted source:
	// the slug ends up in raw SQL after safeSlugRe rejection but never reaches
	// it as user-controlled input.
	TenantIDFunc func(ctx context.Context) string
}

// implicitTenant is the slug a single-tenant deployment runs as. It names no
// database of its own.
//
// PostgreSQL never had to care: Apply sets `search_path = "tenant_default",
// public`, and a search path naming a schema that does not exist simply falls
// through to public. MySQL and MSSQL have no such fallback, so `USE tenant_default`
// is an error, and the request fails before it reaches a handler:
//
//	Error 1049 (42000): Unknown database 'tenant_default'
//
// Belonging on the engine's own database is the same thing PostgreSQL does by
// falling through to public. Apply puts the connection there rather than
// leaving it alone, because "alone" means whatever the previous request did to
// it. A real tenant still switches, and a real tenant whose database is missing
// still fails.
const implicitTenant = "default"

// NewPostgresSchemaTenancy returns a Tenancy that puts every request into the
// `tenant_<slug>` schema for the duration of its connection.
func NewPostgresSchemaTenancy(tenantIDFunc func(ctx context.Context) string) Tenancy {
	return &PostgresSchemaTenancy{TenantIDFunc: tenantIDFunc}
}

// Apply sets search_path on conn to the tenant schema. Returns an error when
// the slug fails validation. Queries after a failed Apply MUST NOT execute
// (the middleware aborts the request).
//
// An unscoped acquisition resets search_path rather than leaving it alone. A
// pooled connection arrives carrying whatever the previous borrower set, so
// doing nothing here means an unscoped request reads the last tenant's schema
// for as long as that connection survives. The MySQL and SQL Server
// strategies re-pin to their default database for the same reason.
func (p *PostgresSchemaTenancy) Apply(ctx context.Context, conn *sql.Conn) error {
	if p == nil || p.TenantIDFunc == nil {
		return nil
	}
	slug := p.TenantIDFunc(ctx)
	if slug == "" {
		// A nil conn is the caller saying there is nothing to scope, and the
		// MySQL path tolerates it for the same reason.
		if conn == nil {
			return nil
		}
		if _, err := conn.ExecContext(ctx, "RESET search_path"); err != nil {
			return fmt.Errorf("postgres tenancy apply unscoped: %w", err)
		}
		return nil
	}
	if !safeSlugRe.MatchString(slug) {
		return fmt.Errorf("%w: invalid tenant slug: %q", core.ErrValidation, slug)
	}
	schema := quoteIdentifier(sqlx.TenantSchemaName(slug))
	_, err := conn.ExecContext(ctx, "SET search_path = "+schema+", public")
	if err != nil {
		return fmt.Errorf("postgres tenancy apply %q: %w", slug, err)
	}
	return nil
}

// Reset clears search_path on conn. Best-effort: the connection close path
// also resets session state, but resetting explicitly avoids surprises if the
// driver ever decides to keep the connection alive in some edge case.
func (p *PostgresSchemaTenancy) Reset(ctx context.Context, conn *sql.Conn) error {
	if p == nil {
		return nil
	}
	if conn == nil {
		return nil
	}
	_, err := conn.ExecContext(ctx, "RESET search_path")
	if err != nil {
		return fmt.Errorf("postgres tenancy reset: %w", err)
	}
	return nil
}

// quoteIdentifier wraps a Postgres identifier in double quotes, doubling any
// embedded quotes. safeSlugRe already rejects slugs containing quotes, so the
// escape is defense-in-depth for any future caller that bypasses the regex.
func quoteIdentifier(s string) string {
	return sqlx.QuotePG(s)
}

// quoteMySQLIdentifier wraps a MySQL identifier in backticks, doubling any
// embedded backticks (MySQL's escape rule). safeSlugRe rejects backticks, so
// like quoteIdentifier this is defense-in-depth.
func quoteMySQLIdentifier(s string) string {
	return sqlx.QuoteMySQL(s)
}

// quoteMSSQLIdentifier wraps an MSSQL identifier in square brackets, doubling
// any embedded close-bracket (MSSQL's escape rule). safeSlugRe rejects
// brackets, so this is defense-in-depth.
func quoteMSSQLIdentifier(s string) string {
	return sqlx.QuoteMSSQL(s)
}

// DatabaseNameOf reports the database the pool's DSN selected, for the
// database-per-tenant strategies that have to be able to put a connection back
// on it. Empty when the pool does not track a name, which the strategies treat
// as "leave the connection where it is".
//
// An optional interface rather than a DB method so that the many test doubles
// implementing DB do not all have to grow a method none of them can answer.
// The cost of that choice is that a wrapper which forgets to forward the method
// silently disables re-pinning, and the runtime pool is wrapped before anything
// asks: every DB decorator in this repository has to forward it, and the miss
// is logged here rather than discovered during a tenant teardown.
func DatabaseNameOf(pool DB) string {
	namer, ok := pool.(interface{ DatabaseName() string })
	if !ok {
		slog.Error("db pool does not report its own database name, so tenant connections cannot be re-pinned to it; a dropped tenant database will strand pooled connections",
			"pool_type", fmt.Sprintf("%T", pool))
		return ""
	}
	return namer.DatabaseName()
}

// PinConnToEngineDB re-pins conn to the engine's own database on the
// database-per-tenant engines (MySQL, MSSQL).
//
// The engine's own database is where the shared catalog (sys_*) tables and
// the shared generated content tables (_*) actually live. A connection the pool
// hands out may still be bound to a tenant database that was dropped mid-flight:
// MySQL does not close a session whose default database was dropped, so the
// connection returns to the pool stranded on a dead name and the next request
// fails with Error 1049 "Unknown database tenant_<slug>" (or Error 1046 "No
// database selected" when the drop happened on that same session). The tenancy
// middleware re-pins only on the tenant-scoped API router. Code that runs
// unscoped (the admin router, schema DDL, migration runners) must re-pin itself
// before it runs a bare (unqualified) table reference.
//
// Postgres is a no-op: it isolates by search_path, not by database, and a
// fresh connection is always on the DSN's default search_path. An empty or
// unusable engine database name is also a no-op, matching the degradation the
// tenancy strategies chose when the server would not report its name.
func PinConnToEngineDB(ctx context.Context, pool DB, conn *sql.Conn) error {
	if conn == nil {
		return nil
	}
	name := DatabaseNameOf(pool)
	if !safeDatabaseNameRe.MatchString(name) {
		return nil
	}
	switch pool.Engine() {
	case "mysql":
		if _, err := conn.ExecContext(ctx, "USE "+quoteMySQLIdentifier(name)); err != nil {
			return fmt.Errorf("pin mysql connection to engine database %q: %w", name, err)
		}
	case "mssql":
		if _, err := conn.ExecContext(ctx, "USE "+quoteMSSQLIdentifier(name)); err != nil {
			return fmt.Errorf("pin mssql connection to engine database %q: %w", name, err)
		}
	}
	return nil
}

// Absent per-tenant databases.

// missingTenantDB records the tenant databases this process has already found
// missing, so the USE that cannot succeed is issued once per slug rather than
// once per request.
//
// A tenant gets no database of its own on MySQL or SQL Server, because no read
// can resolve inside one: the pool names the engine's own database on every
// sys_* reference, and the generated content tables carry the same
// qualification for the same reason. A deployment that has one anyway,
// provisioned by a clone or an import that put tables there, keeps using it.
// Attempting the USE is what tells the two apart, and it costs one statement
// per slug for the lifetime of the process.
//
// The record is negative only. A database that appears after this process has
// decided a slug has none goes unnoticed until the process restarts, which is
// deliberate: nothing the engine reads on these engines resolves inside a
// tenant database, so a stale entry cannot hide a row from anybody. A database
// that disappears needs no record at all, because the USE simply starts failing
// and the request behind it falls back.
type missingTenantDB struct {
	mu sync.RWMutex
	m  map[string]struct{}
}

// known reports whether slug has already been found to have no database.
func (t *missingTenantDB) known(slug string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.m[slug]
	return ok
}

// record marks slug as having no database of its own.
func (t *missingTenantDB) record(slug string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = make(map[string]struct{})
	}
	t.m[slug] = struct{}{}
}

// isMissingDatabaseErr reports whether err is the engine saying that the
// database a USE named is not there.
//
// Matched on the code and never on the message. MySQL 1044 and SQL Server 916
// both describe a database the caller may not open rather than one that does
// not exist, and reading either as absence would put a request that should have
// failed onto the engine's own database instead.
func isMissingDatabaseErr(err error, engine string) bool {
	switch engine {
	case "mysql":
		var myErr *mysql.MySQLError
		return errors.As(err, &myErr) && myErr.Number == 1049
	case "mssql", "sqlserver":
		var msErr mssql.Error
		return errors.As(err, &msErr) && msErr.Number == 911
	}
	return false
}

// MySQL: database-per-tenant isolation via USE.

// MySQLDatabaseTenancy puts each request on the database its tenant runs
// against. Apply runs a USE statement with the backtick-quoted database name on
// the connection. Both Apply and Reset restore DefaultDB for a request that
// belongs to no tenant, so neither a stranded connection nor a dropped tenant
// database can decide which database the next request runs against. MySQL's USE
// persists for the connection's lifetime, so nothing else will undo it.
//
// A tenant with no database of its own runs on DefaultDB, which is where every
// table it can read lives anyway: the pool names the engine's own database on
// each sys_* reference and the generated content tables carry the same
// qualification, so an unqualified name resolving inside tenant_<slug> would
// find nothing there to read. That is the same fall-through Postgres gets from
// search_path, and the tenant_id predicate on every row is what separates one
// tenant from the next on all three engines.
type MySQLDatabaseTenancy struct {
	TenantIDFunc func(ctx context.Context) string
	// DefaultDB is the engine's own database, as the server names it. Empty
	// disables re-pinning: the connection is left wherever it was found.
	DefaultDB string

	// missing remembers the slugs that have no database of their own, so the
	// doomed USE is issued once each rather than once per request.
	missing missingTenantDB
}

// NewMySQLDatabaseTenancy returns a Tenancy that isolates each request to its
// tenant's MySQL database via USE.
func NewMySQLDatabaseTenancy(tenantIDFunc func(ctx context.Context) string, defaultDB string) Tenancy {
	return &MySQLDatabaseTenancy{TenantIDFunc: tenantIDFunc, DefaultDB: defaultDB}
}

// Apply executes `USE tenant_<slug>` to put the connection on the tenant's own
// MySQL database, or puts it back on the engine's own database when the request
// belongs to no tenant, belongs to the implicit one, or belongs to a tenant
// that has no database.
//
// The implicit tenant does not get `USE tenant_default`, because no such
// database exists, but it must still get a USE. A pooled connection cannot be
// assumed to be where it was left:
//
//   - Dropping a database does not close the sessions whose default database it
//     was. A same-session DROP leaves DATABASE() NULL and every unqualified
//     reference failing with 1046 "No database selected". A DROP from another
//     session leaves the dead name in place, so references fail with 1049, or
//     silently resolve against a same-named database created later.
//   - Reset is best-effort by contract, and its caller discards the error. A
//     connection can therefore return to the pool still bound to a tenant.
//
// Either way database/sql hands that connection to whichever request comes
// next. Reasserting here is what makes every acquisition start from known
// state, which is what the Postgres path has always got from re-running SET
// search_path unconditionally.
func (m *MySQLDatabaseTenancy) Apply(ctx context.Context, conn *sql.Conn) error {
	if m == nil || m.TenantIDFunc == nil {
		return nil
	}
	slug := m.TenantIDFunc(ctx)
	if slug == "" || slug == implicitTenant {
		return m.useDefaultDB(ctx, conn, "apply")
	}
	if !safeSlugRe.MatchString(slug) {
		return fmt.Errorf("%w: invalid tenant slug: %q", core.ErrValidation, slug)
	}
	if m.missing.known(slug) {
		return m.useDefaultDB(ctx, conn, "apply")
	}
	_, err := conn.ExecContext(ctx, "USE "+quoteMySQLIdentifier(sqlx.TenantSchemaName(slug)))
	if err == nil {
		return nil
	}
	if !isMissingDatabaseErr(err, "mysql") {
		return fmt.Errorf("mysql tenancy apply %q: %w", slug, err)
	}
	m.missing.record(slug)
	return m.useDefaultDB(ctx, conn, "apply")
}

// Reset restores the connection to DefaultDB so it is not tenant-bound when
// returned to the pool.
func (m *MySQLDatabaseTenancy) Reset(ctx context.Context, conn *sql.Conn) error {
	if m == nil {
		return nil
	}
	return m.useDefaultDB(ctx, conn, "reset")
}

// useDefaultDB pins conn to the engine's own database.
//
// An unusable DefaultDB is a no-op rather than an error. Failing the request
// would turn a misconfigured or unreported database name into a total outage.
func (m *MySQLDatabaseTenancy) useDefaultDB(ctx context.Context, conn *sql.Conn, op string) error {
	if conn == nil || !safeDatabaseNameRe.MatchString(m.DefaultDB) {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "USE "+quoteMySQLIdentifier(m.DefaultDB)); err != nil {
		return fmt.Errorf("mysql tenancy %s default database %q: %w", op, m.DefaultDB, err)
	}
	return nil
}

// MSSQL: database-per-tenant isolation via USE.

// MSSQLDatabaseTenancy puts each request on the database its tenant runs
// against. Same model as MySQL: Apply runs `USE [tenant_<slug>]`, and a tenant
// with no database of its own runs on DefaultDB alongside every table it can
// read.
type MSSQLDatabaseTenancy struct {
	TenantIDFunc func(ctx context.Context) string
	DefaultDB    string

	// missing remembers the slugs that have no database of their own. See the
	// MySQL strategy's field of the same name.
	missing missingTenantDB
}

// NewMSSQLDatabaseTenancy returns a Tenancy that isolates each request to its
// tenant's MSSQL database via USE.
func NewMSSQLDatabaseTenancy(tenantIDFunc func(ctx context.Context) string, defaultDB string) Tenancy {
	return &MSSQLDatabaseTenancy{TenantIDFunc: tenantIDFunc, DefaultDB: defaultDB}
}

// Apply executes `USE [tenant_<slug>]` to put the connection on the tenant's
// own MSSQL database, or puts it back on the engine's own database when the
// request belongs to no tenant, belongs to the implicit one, or belongs to a
// tenant that has no database.
//
// Neither of the two ways a MySQL connection strands itself reaches SQL Server,
// and the reassert is deliberate anyway:
//
//   - Dropping a database means kicking its sessions out first
//     (SET SINGLE_USER WITH ROLLBACK IMMEDIATE), so a connection bound to one
//     is killed rather than handed on broken.
//   - go-mssqldb implements database/sql's ResetSession by setting the TDS
//     reset-connection bit on the next packet, which returns the session to the
//     login's default database, so a stranded tenant binding does not survive
//     the pool round trip either.
//
// Both of those are the driver's property and the server's, not this package's,
// and what they are holding up is a tenant isolation boundary whose failure
// mode is a silent read of another tenant's data rather than an error. One
// statement per acquisition buys the same guarantee out of code that is read
// here, and keeps one rule across all three engines instead of an exception
// that has to be re-verified on every driver upgrade.
func (m *MSSQLDatabaseTenancy) Apply(ctx context.Context, conn *sql.Conn) error {
	if m == nil || m.TenantIDFunc == nil {
		return nil
	}
	slug := m.TenantIDFunc(ctx)
	if slug == "" || slug == implicitTenant {
		return m.useDefaultDB(ctx, conn, "apply")
	}
	if !safeSlugRe.MatchString(slug) {
		return fmt.Errorf("%w: invalid tenant slug: %q", core.ErrValidation, slug)
	}
	if m.missing.known(slug) {
		return m.useDefaultDB(ctx, conn, "apply")
	}
	_, err := conn.ExecContext(ctx, "USE "+quoteMSSQLIdentifier(sqlx.TenantSchemaName(slug)))
	if err == nil {
		return nil
	}
	if !isMissingDatabaseErr(err, "mssql") {
		return fmt.Errorf("mssql tenancy apply %q: %w", slug, err)
	}
	m.missing.record(slug)
	return m.useDefaultDB(ctx, conn, "apply")
}

// Reset restores the connection to DefaultDB so it is not tenant-bound when
// returned to the pool.
func (m *MSSQLDatabaseTenancy) Reset(ctx context.Context, conn *sql.Conn) error {
	if m == nil {
		return nil
	}
	return m.useDefaultDB(ctx, conn, "reset")
}

// useDefaultDB pins conn to the engine's own database. An unusable DefaultDB is
// a no-op for the same reason as on the MySQL path.
func (m *MSSQLDatabaseTenancy) useDefaultDB(ctx context.Context, conn *sql.Conn, op string) error {
	if conn == nil || !safeDatabaseNameRe.MatchString(m.DefaultDB) {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "USE "+quoteMSSQLIdentifier(m.DefaultDB)); err != nil {
		return fmt.Errorf("mssql tenancy %s default database %q: %w", op, m.DefaultDB, err)
	}
	return nil
}

// Per-request connection plumbing.

type tenantConnKey struct{}

// WithTenantConn returns ctx carrying conn as the per-request tenant-isolated
// connection. The DB wrapper's QueryRow/Query/Exec/Begin will use this conn
// instead of pulling fresh ones from the pool, preserving the SET search_path
// applied by the Tenancy strategy.
func WithTenantConn(ctx context.Context, conn *sql.Conn) context.Context {
	return context.WithValue(ctx, tenantConnKey{}, conn)
}

// TenantConn returns the tenant connection stored on ctx, or nil if no
// tenant scope is active. Exported so middleware and tests can introspect.
func TenantConn(ctx context.Context) *sql.Conn {
	c, _ := ctx.Value(tenantConnKey{}).(*sql.Conn)
	return c
}

// LazyTenantConn: lazy tenant-scoped connection acquisition

// LazyTenantConn enables lazy acquisition of a tenant-isolated *sql.Conn.
// Instead of pinning a pooled connection at request start (before the handler
// runs), the TenancyConn middleware stores a LazyTenantConn on the request
// context. The first DB operation in the handler triggers Acquire, which
// obtains a dedicated *sql.Conn and applies tenant isolation. Subsequent
// operations on the same request reuse the conn.
//
// Requests that never touch the database never acquire a conn, freeing pool
// capacity for requests that actually need it.
type LazyTenantConn struct {
	mu      sync.Mutex
	db      *sql.DB
	tenancy Tenancy
	conn    *sql.Conn
	err     error
	once    bool
}

// NewLazyTenantConn returns a LazyTenantConn that acquires from db and applies
// tenancy on the first call to Acquire.
func NewLazyTenantConn(db *sql.DB, tenancy Tenancy) *LazyTenantConn {
	return &LazyTenantConn{db: db, tenancy: tenancy}
}

// Acquire obtains a dedicated *sql.Conn and applies tenant isolation on the
// first call. Subsequent calls return the cached conn+error. Safe for
// concurrent use.
//
// The slow path (first call) acquires the DB connection outside the mutex so
// that pool-pressure-induced blocking does not serialize all first-time DB
// accesses. A double-check after re-acquiring the mutex prevents duplicate
// connections when multiple goroutines race on the first call.
func (l *LazyTenantConn) Acquire(ctx context.Context) (*sql.Conn, error) {
	// Fast path: already acquired.
	l.mu.Lock()
	if l.once {
		defer l.mu.Unlock()
		return l.conn, l.err
	}
	l.mu.Unlock()

	// Slow path: acquire connection outside the lock so pool blocking
	// does not serialize other callers.
	conn, err := l.db.Conn(ctx)
	if err != nil {
		l.mu.Lock()
		if !l.once {
			l.once = true
			l.err = err
		}
		l.mu.Unlock()
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Double-check: another goroutine may have won the race.
	if l.once {
		conn.Close()
		return l.conn, l.err
	}

	l.once = true
	l.err = l.tenancy.Apply(ctx, conn)
	if l.err != nil {
		conn.Close()
		return nil, l.err
	}
	l.conn = conn
	return l.conn, nil
}

// Acquired returns the conn if it was already acquired, or nil. Does NOT
// trigger acquisition: use Acquire for that. Intended for cleanup in
// middleware defers.
func (l *LazyTenantConn) Acquired() *sql.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn
}

type lazyTenantConnKey struct{}

// WithLazyTenantConn returns ctx carrying lc. Downstream DB methods use
// LazyTenantConnFromCtx to lazily acquire a tenant-isolated conn.
func WithLazyTenantConn(ctx context.Context, lc *LazyTenantConn) context.Context {
	return context.WithValue(ctx, lazyTenantConnKey{}, lc)
}

// LazyTenantConnFromCtx returns the LazyTenantConn stored on ctx, or nil.
func LazyTenantConnFromCtx(ctx context.Context) *LazyTenantConn {
	lc, _ := ctx.Value(lazyTenantConnKey{}).(*LazyTenantConn)
	return lc
}
