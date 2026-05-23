package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // MySQL driver registration
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver registration
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	mssql "github.com/microsoft/go-mssqldb"
)

// Connect opens a database connection pool to the given DSN with optional
// maxConns limit. It infers the engine from the DSN scheme, applies a
// 1-hour connection lifetime, and pings to fail fast on a bad DSN.
func Connect(ctx context.Context, dsn string, maxConns int32) (DB, error) {
	return ConnectWithOptions(ctx, dsn, ConnectOptions{
		MaxConns:        maxConns,
		MinConns:        2,
		ConnMaxLifetime: 1 * time.Hour,
		ConnMaxIdleTime: 5 * time.Minute,
	})
}

// ConnectWithOptions opens the database via pgx/v5/stdlib (driver name "pgx"),
// applies pool tuning, pings to fail fast on a bad DSN, and returns the DB
// interface. The engine is inferred from the DSN scheme and drives the
// dialect-aware placeholder rewrite inside the impl.
func ConnectWithOptions(ctx context.Context, dsn string, opts ConnectOptions) (DB, error) {
	engine := EngineFromDSN(dsn)
	d, err := dialect.New(engine)
	if err != nil {
		return nil, fmt.Errorf("dialect for %s: %w", engine, err)
	}

	if opts.StatementTimeout <= 0 {
		opts.StatementTimeout = 30 * time.Second
	}
	// Prevents indefinite hangs when the database is unreachable.
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 10 * time.Second
	}
	dsn = applyConnectTimeout(dsn, engine, opts.ConnectTimeout)

	dsn = applyStatementTimeout(dsn, engine, opts.StatementTimeout)

	// MSSQL: use sql.OpenDB with a connector wrapper that sets LOCK_TIMEOUT
	// on each new connection. PG and MySQL use DSN-level parameters instead.
	var db *sql.DB
	switch engine {
	case "mssql":
		connector, err := mssql.NewConnector(dsn)
		if err != nil {
			return nil, fmt.Errorf("mssql connector: %w", err)
		}
		var wrapped driver.Connector = connector
		if opts.StatementTimeout > 0 {
			wrapped = &mssqlLockTimeoutConnector{
				parent:  connector,
				timeout: opts.StatementTimeout,
			}
		}
		db = sql.OpenDB(wrapped)
	default:
		db, err = sql.Open(driverNameFor(engine), driverDSN(engine, dsn))
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
	}
	if opts.MaxConns > 0 {
		db.SetMaxOpenConns(int(opts.MaxConns))
		// Keep the idle pool the same size as the open pool. Setting
		// MaxIdleConns below MaxOpenConns makes database/sql close and
		// reopen connections on every load burst (churn), which under
		// parallel load shows up as pool-health warnings and 503s.
		db.SetMaxIdleConns(int(opts.MaxConns))
	}
	// Connection lifetime: prevents connection-stickiness to decommissioned
	// backend servers and reduces memory fragmentation from long-lived TCP sessions.
	if opts.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(opts.ConnMaxLifetime)
	} else {
		db.SetConnMaxLifetime(1 * time.Hour) // safe default
	}
	// Idle connection timeout: reaps idle connections, freeing backend slots.
	if opts.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(opts.ConnMaxIdleTime)
	} else {
		db.SetConnMaxIdleTime(5 * time.Minute) // safe default
	}
	// An explicit HealthCheckPeriod sets ConnMaxIdleTime.
	if opts.HealthCheckPeriod > 0 {
		db.SetConnMaxIdleTime(opts.HealthCheckPeriod)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	warnIfPoolCrowdsServer(ctx, db, engine, opts.MaxConns)

	return &sqlDB{db: db, dialect: d, dbName: currentDatabase(ctx, db, engine)}, nil
}

// currentDatabase asks the server which database the DSN landed on, for the
// engines whose tenancy strategy has to be able to put a connection back on it.
//
// Asked rather than parsed out of the DSN because there are three DSN grammars
// and the answer has to be exactly the name the server will accept in a USE.
// One query at boot. The result never changes for the life of the pool.
//
// A server that will not answer is not a reason to refuse to start: the tenancy
// strategy degrades to leaving the connection where it found it.
func currentDatabase(ctx context.Context, db *sql.DB, engine string) string {
	var q string
	switch engine {
	case "mysql":
		q = "SELECT DATABASE()"
	case "mssql":
		q = "SELECT DB_NAME()"
	default:
		return ""
	}
	var name sql.NullString
	if err := db.QueryRowContext(ctx, q).Scan(&name); err != nil {
		slog.WarnContext(ctx, "could not read the engine's own database name; tenant connections will not be re-pinned to it",
			"engine", engine, "error", err)
		return ""
	}
	return name.String
}

// poolCrowdingRatio is the share of the server's connection limit a single
// instance may claim before the pool is worth a warning. A third leaves room
// for two more instances plus the operator's own psql session.
const poolCrowdingRatio = 0.34

// warnIfPoolCrowdsServer logs when this instance's pool is a large fraction of
// what the server will accept.
//
// Exhausting the limit does not fail here, it fails on whichever instance
// connects next, with "sorry, too many clients already" and no indication that
// another instance took the slots. Reading the server's own setting turns
// that into one line at boot on the instance responsible.
//
// Advisory only: an operator who has sized the database for it should not be
// blocked from running a wide pool, and a server that will not answer the
// question is not a reason to refuse to start.
func warnIfPoolCrowdsServer(ctx context.Context, db *sql.DB, engine string, maxConns int32) {
	q := serverMaxConnQuery(engine)
	if q == "" || maxConns <= 0 {
		return
	}
	var serverMax int
	if err := db.QueryRowContext(ctx, q).Scan(&serverMax); err != nil {
		return
	}
	if !poolCrowdsServer(maxConns, serverMax) {
		return
	}
	slog.Warn("database connection pool claims a large share of the server limit",
		"engine", engine,
		"pool_max_conns", maxConns,
		"server_max_connections", serverMax,
		"instances_that_fit", serverMax/int(maxConns),
		"hint", "lower DATABASE_MAX_CONNECTIONS or raise the server limit before scaling out")
}

// serverMaxConnQuery returns the statement reading the server's connection
// limit, or "" for an engine that exposes none.
//
// Each returns a single integer-valued row: PostgreSQL 16 reports 100, MySQL
// 8.0 reports 151, SQL Server reports 32767 for its dynamic default.
func serverMaxConnQuery(engine string) string {
	switch engine {
	case "postgres", "postgresql":
		return "SELECT setting FROM pg_settings WHERE name = 'max_connections'"
	case "mysql":
		return "SELECT @@max_connections"
	case "mssql":
		return "SELECT @@MAX_CONNECTIONS"
	default:
		return ""
	}
}

// poolCrowdsServer reports whether a pool of maxConns claims a large enough
// share of a serverMax-connection server to be worth warning about. A server
// that does not report a usable limit is never crowded.
func poolCrowdsServer(maxConns int32, serverMax int) bool {
	if maxConns <= 0 || serverMax <= 0 {
		return false
	}
	return float64(maxConns) > float64(serverMax)*poolCrowdingRatio
}

// MigrationsTable is where the engine records the newest of its own
// migrations applied, one row as golang-migrate keeps it. It is named apart
// from golang-migrate's default and from each plugin's
// plugin_<name>_schema_migrations, so no other migrator reads or writes it.
const MigrationsTable = "engine_schema_migrations"

// VersionSeed reports the engine schema version a database already holds when
// MigrationsTable is empty, or false when it holds none. Migrate asks it once,
// before the table records anything, so a build can carry the version of an
// install that recorded it some other way. A seed that errors stops the boot,
// because guessing a schema version is how a migration runs twice.
type VersionSeed func(ctx context.Context, db *sql.DB, engine string) (uint, bool, error)

// Migrate applies pending up-migrations for whatever engine the dsn targets,
// reading from the matching subdirectory of migrationsRoot: postgres -> psql,
// mysql -> mysql, mssql -> mssql. The engine is detected from the connection
// string at boot, so one MIGRATIONS_PATH covers every engine without extra
// config. Returns the number of migrations applied (0 = already current).
//
// An advisory lock is acquired before running migrations to prevent two
// instances from racing during concurrent startup.
func Migrate(dsn, migrationsRoot string, seed VersionSeed) (int, error) {
	engine := EngineFromDSN(dsn)
	abs, err := filepath.Abs(filepath.Join(migrationsRoot, engineDir(engine)))
	if err != nil {
		return 0, fmt.Errorf("resolve migrations path: %w", err)
	}

	// Acquire advisory lock to prevent dual-instance migration races.
	migrateDSN := dsn
	if !strings.Contains(dsn, "://") {
		migrateDSN = engine + ":" + "/" + "/" + dsn
	}
	lockDB, err := sql.Open(driverNameFor(engine), driverDSN(engine, migrateDSN))
	if err != nil {
		return 0, fmt.Errorf("open lock connection: %w", err)
	}
	defer lockDB.Close()

	lockConn, err := lockDB.Conn(context.Background())
	if err != nil {
		return 0, fmt.Errorf("acquire lock connection: %w", err)
	}
	defer lockConn.Close()

	if err := waitForMigrationLock(context.Background(), lockConn, engine); err != nil {
		return 0, err
	}
	// Lock auto-releases when lockConn is closed on function return.

	m, err := migrate.New("file://"+abs, withMigrationsTable(migrateDSN))
	if err != nil {
		return 0, fmt.Errorf("create migrator (%s): %w", engine, err)
	}
	defer m.Close()

	if seed != nil {
		if _, _, verr := m.Version(); errors.Is(verr, migrate.ErrNilVersion) {
			v, ok, err := seed(context.Background(), lockDB, engine)
			if err != nil {
				return 0, fmt.Errorf("seed schema version: %w", err)
			}
			if ok {
				if err := m.Force(int(v)); err != nil {
					return 0, fmt.Errorf("record seeded schema version %d: %w", v, err)
				}
			}
		}
	}

	before, _, _ := m.Version()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, fmt.Errorf("run migrations: %w", err)
	}
	after, _, _ := m.Version()
	return int(after) - int(before), nil
}

// withMigrationsTable points golang-migrate at MigrationsTable. Every driver
// the engine uses reads the table name from the same query parameter.
func withMigrationsTable(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "x-migrations-table=" + MigrationsTable
}

// migrationLockWait bounds how long a starting instance waits for another
// instance to finish migrating. Long enough for a large migration on a cold
// database, short enough that a genuinely stuck lock surfaces as a boot failure
// rather than a hang.
const migrationLockWait = 3 * time.Minute

// migrationLockPoll is the gap between acquisition attempts. All three engines
// expose only a try-acquire that fits one statement, so the wait is a poll.
const migrationLockPoll = time.Second

// waitForMigrationLock blocks until the migration advisory lock is held, the
// context is done, or migrationLockWait elapses.
//
// It waits rather than failing on the first refusal because every replicated
// deployment starts its instances at once. A try-acquire treated as fatal would
// let the first instance migrate and abort boot on the rest, so a Deployment
// with replicas > 1 would crash-loop every pod but one on its first rollout.
// The waiters find nothing left to apply once the holder is done and continue
// normally.
func waitForMigrationLock(ctx context.Context, conn *sql.Conn, engine string) error {
	deadline := time.Now().Add(migrationLockWait)
	for attempt := 0; ; attempt++ {
		locked, err := tryMigrationLock(ctx, conn, engine)
		if err != nil {
			return fmt.Errorf("acquire advisory lock: %w", err)
		}
		if locked {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("migration lock held by another instance for longer than %s", migrationLockWait)
		}
		if attempt == 0 {
			slog.Info("migration lock held by another instance, waiting",
				"engine", engine, "timeout", migrationLockWait.String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(migrationLockPoll):
		}
	}
}

// tryMigrationLock attempts a single non-blocking acquire on the engine's
// advisory lock, reporting whether it was granted.
func tryMigrationLock(ctx context.Context, conn *sql.Conn, engine string) (bool, error) {
	switch engine {
	case "postgres":
		var locked bool
		if err := conn.QueryRowContext(ctx,
			"SELECT pg_try_advisory_lock(hashtext('lyeve_migration'))").Scan(&locked); err != nil {
			return false, err
		}
		return locked, nil
	case "mysql":
		// GET_LOCK names are server-wide and capped at 64 characters, so the
		// lock is keyed by a digest of the target schema. Two instances
		// migrating different databases on one server must not block each other.
		var result sql.NullInt64
		if err := conn.QueryRowContext(ctx,
			"SELECT GET_LOCK(CONCAT('lyeve_migration_', MD5(DATABASE())), 0)").Scan(&result); err != nil {
			return false, err
		}
		return result.Valid && result.Int64 == 1, nil
	case "mssql":
		var code int
		if err := conn.QueryRowContext(ctx,
			"DECLARE @result INT; EXEC @result = sp_getapplock @Resource='lyeve_migration', @LockMode='Exclusive', @LockOwner='Session', @LockTimeout=0; SELECT @result").Scan(&code); err != nil {
			return false, err
		}
		return code >= 0, nil
	default:
		// No advisory lock for this engine: proceed rather than block forever.
		return true, nil
	}
}

// EngineFromDSN identifies the database engine a connection string targets, from
// its URL scheme: postgres/postgresql, mysql, or sqlserver/mssql. For go-sql-
// driver/mysql (user:***@tcp(host:port)/dbname, no scheme prefix), the
// @tcp( marker is detected. Anything else (including keyword-form Postgres DSNs like
// "host=... dbname=...") falls back to postgres.
func EngineFromDSN(dsn string) string {
	scheme := dsn
	if i := strings.Index(scheme, "://"); i >= 0 {
		scheme = strings.ToLower(scheme[:i])
	}
	switch scheme {
	case "mysql":
		return "mysql"
	case "sqlserver", "mssql":
		return "mssql"
	default:
		// go-sql-driver/mysql DSN format: user:***@tcp(host:port)/dbname
		if strings.Contains(dsn, "@tcp(") {
			return "mysql"
		}
		return "postgres"
	}
}

// driverDSN converts a golang-migrate URL into the form its database/sql
// driver expects. go-sql-driver/mysql rejects the "mysql://" scheme: it
// reads everything before the first colon as the username, so a schemed DSN
// authenticates as "mysql" and fails. pgx and go-mssqldb both accept URLs
// unchanged.
func driverDSN(engine, dsn string) string {
	if engine == "mysql" {
		return withUTCLocation(withClientFoundRows(withMultiStatements(strings.TrimPrefix(dsn, "mysql://"))))
	}
	return dsn
}

// withClientFoundRows guarantees clientFoundRows=true on a MySQL DSN.
//
// MySQL reports the number of rows an UPDATE *changed*. Postgres and SQL Server
// report the number it *matched*. Callers read RowsAffected == 0 as "no such
// row", so on MySQL an update that writes the values a row already holds would
// be indistinguishable from an update against a row that does not exist, and a
// handler would answer 404 for a row it had just loaded.
//
// The flag makes MySQL count matched rows, which is what every caller already
// assumes and what the other two engines already do.
func withClientFoundRows(dsn string) string {
	if strings.Contains(dsn, "clientFoundRows=") {
		return dsn
	}
	return appendDSNParam(dsn, "clientFoundRows=true")
}

// withUTCLocation holds the MySQL driver's time location at UTC, which is its
// default, by rewriting any loc parameter a DATABASE_URL carries.
//
// A DATETIME column keeps no offset. The driver writes a time.Time as the
// wall clock in its location, and the content read path takes every stored
// datetime to be UTC, because that is the only way three databases can return
// one value. Under loc=Local on a host that is not on UTC, a datetime written
// at 09:30Z would be stored as the host's wall clock and read back hours off.
func withUTCLocation(dsn string) string {
	base, query, ok := strings.Cut(dsn, "?")
	if !ok {
		return dsn
	}
	params := strings.Split(query, "&")
	for i, p := range params {
		if strings.HasPrefix(p, "loc=") {
			params[i] = "loc=UTC"
		}
	}
	return base + "?" + strings.Join(params, "&")
}

// withMultiStatements guarantees multiStatements=true on a MySQL DSN.
//
// Migration files hold several statements each, and PluginMigrate executes a
// file per Exec. go-sql-driver/mysql defaults the flag off, so the driver sends
// only the first statement and MySQL rejects the rest as a syntax error at the
// second one. Forcing it here, rather than documenting it, keeps the engine's
// requirement in the engine, so a stock DATABASE_URL migrates.
func withMultiStatements(dsn string) string {
	if strings.Contains(dsn, "multiStatements=") {
		return dsn
	}
	return appendDSNParam(dsn, "multiStatements=true")
}

// engineDir maps an engine to its migrations subdirectory under the root.
// Postgres is shortened to psql to match the folder layout.
func engineDir(engine string) string {
	switch engine {
	case "mysql":
		return "mysql"
	case "mssql":
		return "mssql"
	default:
		return "psql"
	}
}

// applyStatementTimeout appends the engine-appropriate statement timeout
// parameter to the DSN so every connection in the pool enforces it at the
// session level:
//
//   - Postgres: statement_timeout (milliseconds)
//   - MySQL: max_execution_time (milliseconds)
//   - MSSQL: no-op - handled via connector-based SET LOCK_TIMEOUT in ConnectWithOptions
func applyStatementTimeout(dsn, engine string, timeout time.Duration) string {
	ms := timeout.Milliseconds()
	if ms <= 0 {
		return dsn
	}
	switch engine {
	case "postgres":
		return appendDSNParam(dsn, fmt.Sprintf("statement_timeout=%d", ms))
	case "mysql":
		return appendDSNParam(dsn, fmt.Sprintf("max_execution_time=%d", ms))
	default:
		return dsn // MSSQL: connector-based, not DSN-based
	}
}

// appendDSNParam appends a key=value parameter to a DSN, handling the
// first-parameter vs subsequent-parameter separator.
func appendDSNParam(dsn, param string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + param
	}
	return dsn + "?" + param
}

// mssqlLockTimeoutConnector wraps a driver.Connector to execute SET LOCK_TIMEOUT
// on every new MSSQL connection. LOCK_TIMEOUT controls how long a statement waits
// for a lock before failing (milliseconds). For true query execution timeout,
// callers should use context.WithTimeout: LOCK_TIMEOUT only covers lock waits.
type mssqlLockTimeoutConnector struct {
	parent  driver.Connector
	timeout time.Duration
}

func (c *mssqlLockTimeoutConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.parent.Connect(ctx)
	if err != nil {
		return nil, err
	}
	execConn, ok := conn.(driver.ExecerContext)
	if !ok {
		return conn, nil // connector doesn't support Exec: skip silently
	}
	ms := c.timeout.Milliseconds()
	if _, err := execConn.ExecContext(ctx, fmt.Sprintf("SET LOCK_TIMEOUT %d", ms), nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set mssql lock_timeout: %w", err)
	}
	return conn, nil
}

func (c *mssqlLockTimeoutConnector) Driver() driver.Driver {
	return c.parent.Driver()
}

// applyConnectTimeout appends the engine-appropriate connection timeout
// parameter to the DSN so the driver fails fast instead of hanging indefinitely
// when the database is unreachable:
//
//   - Postgres: connect_timeout (seconds)
//   - MySQL: timeout (Go duration string, e.g. "10s")
//   - MSSQL: dial+timeout (milliseconds)
func applyConnectTimeout(dsn, engine string, timeout time.Duration) string {
	if timeout <= 0 {
		return dsn
	}
	switch engine {
	case "postgres":
		return appendDSNParam(dsn, fmt.Sprintf("connect_timeout=%d", int(timeout.Seconds())))
	case "mysql":
		return appendDSNParam(dsn, fmt.Sprintf("timeout=%s", timeout.String()))
	default: // mssql
		return appendDSNParam(dsn, fmt.Sprintf("dial+timeout=%d", timeout.Milliseconds()))
	}
}

// driverNameFor maps an engine identifier to the database/sql driver name its
// driver package registers when blank-imported:
//
//	postgres -> "pgx"       (github.com/jackc/pgx/v5/stdlib)
//	mysql    -> "mysql"     (github.com/go-sql-driver/mysql)
//	mssql    -> "sqlserver" (github.com/microsoft/go-mssqldb)
//
// All three drivers are blank-imported in this file, so sql.Open works for
// every supported engine.
func driverNameFor(engine string) string {
	switch engine {
	case "mysql":
		return "mysql"
	case "mssql":
		return "sqlserver"
	default:
		return "pgx"
	}
}

// ConnectReplica opens a separate read-only connection pool for the replica DSN
// and attaches it to the given DB. When replicaDSN is empty, this is a no-op
//
//	-- QuerierRO will fall back to the primary pool.
//
// The replica pool is sized independently (replicaMaxConns) and shares the
// same dialect + engine as the primary. A ping is issued to fail fast on a
// bad DSN.
func ConnectReplica(ctx context.Context, pool DB, replicaDSN string, replicaMaxConns int32) error {
	if replicaDSN == "" {
		return nil
	}
	s, ok := pool.(*sqlDB)
	if !ok {
		return fmt.Errorf("connect replica: pool is not *sqlDB")
	}
	engine := EngineFromDSN(replicaDSN)
	if engine != s.dialect.Name() {
		return fmt.Errorf("connect replica: replica engine %q does not match primary engine %q", engine, s.dialect.Name())
	}
	replicaDSN = applyConnectTimeout(replicaDSN, engine, 10*time.Second)
	replicaDSN = applyStatementTimeout(replicaDSN, engine, 30*time.Second)

	// MSSQL: same connector wrapper as ConnectWithOptions.
	var db *sql.DB
	switch engine {
	case "mssql":
		connector, err := mssql.NewConnector(replicaDSN)
		if err != nil {
			return fmt.Errorf("mssql replica connector: %w", err)
		}
		wrapped := &mssqlLockTimeoutConnector{
			parent:  connector,
			timeout: 30 * time.Second,
		}
		db = sql.OpenDB(wrapped)
	default:
		var err error
		db, err = sql.Open(driverNameFor(engine), driverDSN(engine, replicaDSN))
		if err != nil {
			return fmt.Errorf("open replica: %w", err)
		}
	}
	if replicaMaxConns > 0 {
		db.SetMaxOpenConns(int(replicaMaxConns))
		db.SetMaxIdleConns(int(replicaMaxConns))
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return fmt.Errorf("ping replica: %w", err)
	}
	s.replica = db
	return nil
}
