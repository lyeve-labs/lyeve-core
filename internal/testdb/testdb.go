// Package testdb provides testcontainers-backed database helpers for
// integration tests. Each helper returns a db.DB backed by a fresh database
// within a shared container (one per dialect per test binary). The container
// is started once via sync.Once. Each test gets its own database for
// isolation. Postgres uses TEMPLATE for instant DB creation, while MySQL/MSSQL
// create + migrate per test.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // mysql driver for sql.Open
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/database/sqlserver"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"  // pgx driver for sql.Open
	_ "github.com/microsoft/go-mssqldb" // sqlserver driver for sql.Open

	"github.com/testcontainers/testcontainers-go"
	tcmssql "github.com/testcontainers/testcontainers-go/modules/mssql"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// migrationsRoot returns the absolute path to the module's migrations
// directory.
func migrationsRoot() string {
	_, f, _, _ := runtime.Caller(0)
	// internal/testdb/ -> internal/ -> module root
	return filepath.Join(filepath.Dir(f), "..", "..", "migrations")
}

// containerImage resolves the Docker image to use for a given adapter. If the
// CI_IMAGE_<ADAPTER> environment variable is set it overrides the default,
// allowing the CI matrix to inject different versions without code changes.
func containerImage(adapter, defaultImage string) string {
	envKey := "CI_IMAGE_" + adapter
	if img := os.Getenv(envKey); img != "" {
		return img
	}
	return defaultImage
}

// ShouldTest returns true when the current runner should execute tests for the
// given adapter. In CI, CI_DIALECT gates each matrix job to a single dialect.
// Locally (CI_DIALECT unset), all dialects are enabled.
func ShouldTest(adapter string) bool {
	dialect := os.Getenv("CI_DIALECT")
	if dialect == "" {
		return true
	}
	return dialect == adapter
}

// Shared container management
//
// Each dialect has a sync.Once-guarded container that lives for the entire
// test binary process. Per-test databases are created within the shared
// container and dropped on cleanup. This eliminates the dominant cost
// (container startup: 10-30s each) while preserving test isolation.

var dbSeq atomic.Int64 // monotonic counter for unique DB names

// DefaultMaxConns is the pool size every test database gets unless the test
// asks for more.
//
// It is deliberately small. Every suite in a test run shares one container
// per dialect, so a large default multiplies across every package running at
// once and starves them: Postgres ships 100 connection slots and a handful of
// parallel suites would exhaust them.
//
// A test whose subject is concurrency has to ask, because the cap silently
// bounds it: a 20-caller burst against a 4-connection pool is four callers at a
// time and nineteen waiting, so a race that needs more than four simultaneous
// sessions cannot happen and the test passes without ever reproducing it. Use
// the WithMaxConns constructors for those.
const DefaultMaxConns = 4

func nextDBName(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, dbSeq.Add(1))
}

// Postgres

var pgShared struct {
	once     sync.Once
	ctr      *postgres.PostgresContainer
	host     string
	port     string
	adminDSN string // DSN to "postgres" maintenance database (for CREATE/DROP DATABASE)
	tplName  string // template database name (migrations pre-applied)
	// ddlApplied counts the registered DDL entries the template carries.
	// A database cloned from it runs only the entries registered later.
	ddlApplied int
	err        error
}

func ensurePostgres(t *testing.T) {
	t.Helper()
	pgShared.once.Do(func() {
		ctx := context.Background()
		pgShared.ctr, pgShared.err = postgres.Run(ctx,
			containerImage("POSTGRES", "postgres:16-alpine"),
			postgres.WithDatabase("lyeve_test"),
			postgres.WithUsername("lyeve"),
			postgres.WithPassword("secret"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if pgShared.err != nil {
			return
		}

		pgShared.host, pgShared.err = pgShared.ctr.Host(ctx)
		if pgShared.err != nil {
			return
		}
		mp, err := pgShared.ctr.MappedPort(ctx, "5432")
		if err != nil {
			pgShared.err = err
			return
		}
		pgShared.port = mp.Port()

		// Run core migrations on lyeve_test: this becomes a template.
		dsn, err := pgShared.ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			pgShared.err = err
			return
		}
		pgShared.tplName = "lyeve_test"

		n, err := db.Migrate(dsn, migrationsRoot(), nil)
		if err != nil {
			pgShared.err = fmt.Errorf("migrate template db: %w", err)
			return
		}
		_ = n // logged once here. Individual tests don't need migration counts

		// The tables suites registered, applied once here so every clone
		// carries them.
		applied, err := applyRegisteredDDLAt("pgx", dsn, "postgres")
		if err != nil {
			pgShared.err = err
			return
		}
		pgShared.ddlApplied = applied

		// Admin DSN targets the "postgres" maintenance database so we can
		// CREATE DATABASE ... TEMPLATE lyeve_test without conflicting connections.
		pgShared.adminDSN = fmt.Sprintf(
			"postgres://lyeve:secret@%s:%s/postgres?sslmode=disable",
			pgShared.host, pgShared.port,
		)
	})
	if pgShared.err != nil {
		t.Fatalf("shared postgres container: %v", pgShared.err)
	}
}

// pgNewDB creates a fresh Postgres database within the shared container. It
// uses CREATE DATABASE ... TEMPLATE for near-instant creation, with the
// migrations already applied. Returns the database name.
func pgNewDB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dbName := nextDBName("pgtest")

	adminDB, err := sql.Open("pgx", pgShared.adminDSN)
	if err != nil {
		t.Fatalf("pg admin open: %v", err)
	}
	defer adminDB.Close()

	_, err = adminDB.ExecContext(ctx,
		fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", dbName, pgShared.tplName))
	if err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", dbName, err)
	}

	t.Cleanup(func() {
		cleanupDB, err2 := sql.Open("pgx", pgShared.adminDSN)
		if err2 != nil {
			t.Logf("pg cleanup open: %v", err2)
			return
		}
		defer cleanupDB.Close()
		// Terminate lingering connections before dropping.
		_, _ = cleanupDB.ExecContext(context.Background(), // err suppressed: best-effort test cleanup
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = cleanupDB.ExecContext(context.Background(), // err suppressed: best-effort test cleanup
			fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
	})

	return dbName
}

// pgDSN builds a Postgres DSN for the given database name within the shared
// container.
func pgDSN(dbName string) string {
	return fmt.Sprintf("postgres://lyeve:secret@%s:%s/%s?sslmode=disable",
		pgShared.host, pgShared.port, dbName)
}

// Postgres starts a shared postgres:16-alpine container (once per process),
// creates a fresh database from a pre-migrated template, and returns a db.DB
// connected to it. The database and connection are cleaned up when the test
// finishes.
func Postgres(t *testing.T) db.DB {
	t.Helper()
	return PostgresWithMaxConns(t, DefaultMaxConns)
}

// PostgresWithMaxConns is Postgres with the pool sized by the caller. Use it
// when the test is about concurrency: DefaultMaxConns bounds how many callers
// can be in the database at once, which silently caps what a burst test
// reproduces.
func PostgresWithMaxConns(t *testing.T, maxConns int) db.DB {
	t.Helper()
	d, _ := postgresWithDSN(t, maxConns)
	return d
}

// PostgresWithDSN is Postgres that also returns the database's DSN, for a
// test that opens a second pool on the same database the way the engine
// opens its side pools.
func PostgresWithDSN(t *testing.T) (db.DB, string) {
	t.Helper()
	return postgresWithDSN(t, DefaultMaxConns)
}

func postgresWithDSN(t *testing.T, maxConns int) (db.DB, string) {
	t.Helper()
	ensurePostgres(t)

	if maxConns < 1 {
		maxConns = DefaultMaxConns
	}
	dbName := pgNewDB(t) // TEMPLATE: migrations already applied
	d, err := db.Connect(context.Background(), pgDSN(dbName), int32(maxConns))
	if err != nil {
		t.Fatalf("connect to %s: %v", dbName, err)
	}
	t.Cleanup(func() { d.Close() })

	// The template carries what was registered before it was built. Anything
	// registered since goes on this database alone.
	if _, err := applyRegisteredDDL(context.Background(), d.SQLDB(), "postgres", pgShared.ddlApplied); err != nil {
		t.Fatal(err)
	}

	return d, pgDSN(dbName)
}

// MySQL

var mysqlShared struct {
	once sync.Once
	ctr  *mysql.MySQLContainer
	host string
	port string
	err  error
}

func ensureMySQL(t *testing.T) {
	t.Helper()
	mysqlShared.once.Do(func() {
		ctx := context.Background()
		mysqlShared.ctr, mysqlShared.err = mysql.Run(ctx,
			containerImage("MYSQL", "mysql:8"),
			mysql.WithDatabase("lyeve_test"),
			mysql.WithUsername("lyeve"),
			mysql.WithPassword("secret"),
			// Use utf8mb4_unicode_ci, the collation plugin migrations name.
			// Those plugins join against sys_ tables created by the server
			// default, and a stock mysql:8 defaults to utf8mb4_0900_ai_ci, so
			// the joins would fail in the harness with "illegal mix of
			// collations". That would be coverage lost to the harness rather
			// than a product fault.
			testcontainers.WithCmd(
				"--character-set-server=utf8mb4",
				"--collation-server=utf8mb4_unicode_ci",
			),
			testcontainers.WithWaitStrategy(
				wait.ForLog("port: 3306  MySQL Community Server").
					WithStartupTimeout(90*time.Second),
			),
		)
		if mysqlShared.err != nil {
			return
		}
		mysqlShared.host, mysqlShared.err = mysqlShared.ctr.Host(ctx)
		if mysqlShared.err != nil {
			return
		}
		mp, err := mysqlShared.ctr.MappedPort(ctx, "3306")
		if err != nil {
			mysqlShared.err = err
			return
		}
		mysqlShared.port = mp.Port()
	})
	if mysqlShared.err != nil {
		t.Fatalf("shared mysql container: %v", mysqlShared.err)
	}
}

// mysqlRootDSN returns a go-sql-driver/mysql DSN as root for the given
// database (empty string = no database).
func mysqlRootDSN(dbName string) string {
	dbPart := "/"
	if dbName != "" {
		dbPart = "/" + dbName
	}
	// No sql_mode override: the harness has to be the server an operator runs.
	//
	// ANSI_QUOTES, for one, makes MySQL read " as an identifier quote the way
	// PostgreSQL does, so a store that quoted its columns the PostgreSQL way
	// would look correct here and be rejected by a stock server with error
	// 1064. A stock MySQL 8 also sets NO_ZERO_DATE and ONLY_FULL_GROUP_BY,
	// which reject statements a relaxed mode would accept.
	// clientFoundRows matches what driverDSN sets on the engine's own MySQL
	// connection. Without it the harness counts changed rows while production
	// counts matched ones, so an idempotent update looks like a missing row
	// here and not there, or the reverse, which is the same class of mistake
	// as the sql_mode note above.
	return fmt.Sprintf("root:secret@tcp(%s:%s)%s?parseTime=true&multiStatements=true&clientFoundRows=true",
		mysqlShared.host, mysqlShared.port, dbPart)
}

// mysqlMigrateDSN returns a golang-migrate DSN for the given database.
func mysqlMigrateDSN(dbName string) string {
	return fmt.Sprintf("mysql://root:secret@tcp(%s:%s)/%s?multiStatements=true",
		mysqlShared.host, mysqlShared.port, dbName)
}

// MySQL starts a shared mysql:8 container (once per process), creates a fresh
// database, runs core migrations, and returns a db.DB.
func MySQL(t *testing.T) db.DB {
	t.Helper()
	return MySQLWithMaxConns(t, DefaultMaxConns)
}

// MySQLWithMaxConns is MySQL with the pool sized by the caller. See
// PostgresWithMaxConns for when to reach for it.
func MySQLWithMaxConns(t *testing.T, maxConns int) db.DB {
	t.Helper()
	d, _ := mysqlWithDSN(t, maxConns)
	return d
}

// MySQLWithDSN is MySQL that also returns the database's DSN. See
// PostgresWithDSN.
func MySQLWithDSN(t *testing.T) (db.DB, string) {
	t.Helper()
	return mysqlWithDSN(t, DefaultMaxConns)
}

func mysqlWithDSN(t *testing.T, maxConns int) (db.DB, string) {
	t.Helper()
	ensureMySQL(t)

	if maxConns < 1 {
		maxConns = DefaultMaxConns
	}
	ctx := context.Background()
	dbName := nextDBName("mysqltest")

	adminDB, err := sql.Open("mysql", mysqlRootDSN(""))
	if err != nil {
		t.Fatalf("mysql admin open: %v", err)
	}
	defer adminDB.Close()
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("mysql CREATE DATABASE %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		d, err2 := sql.Open("mysql", mysqlRootDSN(""))
		if err2 != nil {
			return
		}
		defer d.Close()
		_, _ = d.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName) // err suppressed: best-effort test cleanup
	})

	d, err := db.Connect(ctx, mysqlRootDSN(dbName), int32(maxConns))
	if err != nil {
		t.Fatalf("connect to mysql %s: %v", dbName, err)
	}
	t.Cleanup(func() { d.Close() })

	n, err := db.Migrate(mysqlMigrateDSN(dbName), migrationsRoot(), nil)
	if err != nil {
		t.Fatalf("migrate mysql %s: %v", dbName, err)
	}
	t.Logf("mysql %s: applied %d migrations", dbName, n)

	if _, err := applyRegisteredDDL(ctx, d.SQLDB(), "mysql", 0); err != nil {
		t.Fatal(err)
	}

	return d, mysqlRootDSN(dbName)
}

// MSSQL

var mssqlShared struct {
	once     sync.Once
	ctr      *tcmssql.MSSQLServerContainer
	host     string
	port     string
	adminDSN string // DSN to master (no database specified)
	err      error
}

func ensureMSSQL(t *testing.T) {
	t.Helper()
	mssqlShared.once.Do(func() {
		ctx := context.Background()
		mssqlShared.ctr, mssqlShared.err = tcmssql.Run(ctx,
			containerImage("MSSQL", "mcr.microsoft.com/azure-sql-edge:latest"),
			tcmssql.WithAcceptEULA(),
			tcmssql.WithPassword("Str0ng@Passw0rd"),
		)
		if mssqlShared.err != nil {
			return
		}
		mssqlShared.host, mssqlShared.err = mssqlShared.ctr.Host(ctx)
		if mssqlShared.err != nil {
			return
		}
		mp, err := mssqlShared.ctr.MappedPort(ctx, "1433")
		if err != nil {
			mssqlShared.err = err
			return
		}
		mssqlShared.port = mp.Port()

		// Store admin DSN (connects to master by default). The password is
		// already URL-encoded by testcontainers.
		mssqlShared.adminDSN, mssqlShared.err = mssqlShared.ctr.ConnectionString(
			ctx, "encrypt=disable", "TrustServerCertificate=true",
		)
	})
	if mssqlShared.err != nil {
		t.Fatalf("shared mssql container: %v", mssqlShared.err)
	}
}

// mssqlDSN returns a SQL Server DSN for the given database within the shared
// container. It appends the database parameter to the admin DSN.
func mssqlDSN(dbName string) string {
	return mssqlShared.adminDSN + "&database=" + dbName
}

// MSSQL starts a shared Azure SQL Edge container (once per process), creates
// a fresh database, runs core migrations, and returns a db.DB.
func MSSQL(t *testing.T) db.DB {
	t.Helper()
	return MSSQLWithMaxConns(t, DefaultMaxConns)
}

// MSSQLWithMaxConns is MSSQL with the pool sized by the caller. See
// PostgresWithMaxConns for when to reach for it.
func MSSQLWithMaxConns(t *testing.T, maxConns int) db.DB {
	t.Helper()
	d, _ := mssqlWithDSN(t, maxConns)
	return d
}

// MSSQLWithDSN is MSSQL that also returns the database's DSN. See
// PostgresWithDSN.
func MSSQLWithDSN(t *testing.T) (db.DB, string) {
	t.Helper()
	return mssqlWithDSN(t, DefaultMaxConns)
}

func mssqlWithDSN(t *testing.T, maxConns int) (db.DB, string) {
	t.Helper()
	ensureMSSQL(t)

	if maxConns < 1 {
		maxConns = DefaultMaxConns
	}
	ctx := context.Background()
	dbName := nextDBName("mssqltest")

	adminDB, err := sql.Open("sqlserver", mssqlShared.adminDSN)
	if err != nil {
		t.Fatalf("mssql admin open: %v", err)
	}
	defer adminDB.Close()
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE [%s]", dbName)); err != nil {
		t.Fatalf("mssql CREATE DATABASE %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		d, err2 := sql.Open("sqlserver", mssqlShared.adminDSN)
		if err2 != nil {
			return
		}
		defer d.Close()
		_, _ = d.ExecContext(context.Background(), // err suppressed: best-effort test cleanup
			fmt.Sprintf("ALTER DATABASE [%s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE", dbName))
		_, _ = d.ExecContext(context.Background(), // err suppressed: best-effort test cleanup
			fmt.Sprintf("DROP DATABASE [%s]", dbName))
	})

	testDSN := mssqlDSN(dbName)
	d, err := db.Connect(ctx, testDSN, int32(maxConns))
	if err != nil {
		t.Fatalf("connect to mssql %s: %v", dbName, err)
	}
	t.Cleanup(func() { d.Close() })

	n, err := db.Migrate(testDSN, migrationsRoot(), nil)
	if err != nil {
		t.Fatalf("migrate mssql %s: %v", dbName, err)
	}
	t.Logf("mssql %s: applied %d migrations", dbName, n)

	if _, err := applyRegisteredDDL(ctx, d.SQLDB(), "mssql", 0); err != nil {
		t.Fatal(err)
	}

	return d, testDSN
}
