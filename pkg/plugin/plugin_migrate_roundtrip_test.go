//go:build integration && !mutest
// +build integration,!mutest

package plugin_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/database/sqlserver"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
	enginedb "github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	_ "github.com/microsoft/go-mssqldb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/testcontainers/testcontainers-go"
	tcmssql "github.com/testcontainers/testcontainers-go/modules/mssql"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Shared containers for roundtrip tests
// One container per dialect, shared across all plugin roundtrip subtests.
// Each test gets its own empty database within the shared container.

var rtDBSeq atomic.Int64

func rtNextDB(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, rtDBSeq.Add(1))
}

// rtFreshDB is an empty database and the DSN that reaches it. The engine's own
// migration runner takes a DSN rather than a pool, so a helper handing back
// only the pool could not lay the base schema down.
type rtFreshDB struct {
	db  *sql.DB
	dsn string
}

// Postgres

var rtPg struct {
	once sync.Once
	ctr  *postgres.PostgresContainer
	host string
	port string
	err  error
}

func rtEnsurePG(t *testing.T) {
	t.Helper()
	rtPg.once.Do(func() {
		ctx := context.Background()
		var err error
		rtPg.ctr, err = postgres.Run(ctx,
			"postgres:16-alpine",
			postgres.WithDatabase("rt_template"),
			postgres.WithUsername("cms"),
			postgres.WithPassword("secret"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if err != nil {
			rtPg.err = err
			return
		}
		rtPg.host, rtPg.err = rtPg.ctr.Host(ctx)
		if rtPg.err != nil {
			return
		}
		mp, err := rtPg.ctr.MappedPort(ctx, "5432")
		if err != nil {
			rtPg.err = err
			return
		}
		rtPg.port = mp.Port()
	})
	if rtPg.err != nil {
		t.Fatalf("roundtrip pg container: %v", rtPg.err)
	}
}

func rtPG(t *testing.T) rtFreshDB {
	t.Helper()
	rtEnsurePG(t)
	ctx := context.Background()
	dbName := rtNextDB("rtpg")

	adminDSN := fmt.Sprintf("postgres://cms:secret@%s:%s/postgres?sslmode=disable", rtPg.host, rtPg.port)
	adminDB, err := sql.Open("pgx", adminDSN)
	require.NoError(t, err)
	defer adminDB.Close()

	_, err = adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		cDB, err2 := sql.Open("pgx", adminDSN)
		if err2 != nil {
			return
		}
		defer cDB.Close()
		_, _ = cDB.ExecContext(context.Background(),
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = cDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
	})

	dsn := fmt.Sprintf("postgres://cms:secret@%s:%s/%s?sslmode=disable", rtPg.host, rtPg.port, dbName)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, db.PingContext(ctx))
	return rtFreshDB{db: db, dsn: dsn}
}

// MySQL

var rtMySQL struct {
	once sync.Once
	ctr  *mysql.MySQLContainer
	host string
	port string
	err  error
}

func rtEnsureMySQL(t *testing.T) {
	t.Helper()
	rtMySQL.once.Do(func() {
		ctx := context.Background()
		var err error
		rtMySQL.ctr, err = mysql.Run(ctx,
			"mysql:8",
			mysql.WithDatabase("rt_template"),
			mysql.WithUsername("cms"),
			mysql.WithPassword("secret"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("port: 3306  MySQL Community Server").
					WithStartupTimeout(90*time.Second),
			),
		)
		if err != nil {
			rtMySQL.err = err
			return
		}
		rtMySQL.host, rtMySQL.err = rtMySQL.ctr.Host(ctx)
		if rtMySQL.err != nil {
			return
		}
		mp, err := rtMySQL.ctr.MappedPort(ctx, "3306")
		if err != nil {
			rtMySQL.err = err
			return
		}
		rtMySQL.port = mp.Port()
	})
	if rtMySQL.err != nil {
		t.Fatalf("roundtrip mysql container: %v", rtMySQL.err)
	}
}

func rtMy(t *testing.T) rtFreshDB {
	t.Helper()
	rtEnsureMySQL(t)
	ctx := context.Background()
	dbName := rtNextDB("rtmy")

	rootDSN := fmt.Sprintf("root:secret@tcp(%s:%s)/?parseTime=true&multiStatements=true",
		rtMySQL.host, rtMySQL.port)
	adminDB, err := sql.Open("mysql", rootDSN)
	require.NoError(t, err)
	defer adminDB.Close()

	_, err = adminDB.ExecContext(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	t.Cleanup(func() {
		d, err2 := sql.Open("mysql", rootDSN)
		if err2 != nil {
			return
		}
		defer d.Close()
		_, _ = d.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
	})

	dbDSN := fmt.Sprintf("root:secret@tcp(%s:%s)/%s?parseTime=true&multiStatements=true",
		rtMySQL.host, rtMySQL.port, dbName)
	db, err := sql.Open("mysql", dbDSN)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, db.PingContext(ctx))

	// The migration runner wants the schemed form, and without parseTime,
	// which its MySQL driver rejects.
	migrateDSN := fmt.Sprintf("mysql://root:secret@tcp(%s:%s)/%s?multiStatements=true",
		rtMySQL.host, rtMySQL.port, dbName)
	return rtFreshDB{db: db, dsn: migrateDSN}
}

// MSSQL

var rtMSSQL struct {
	once     sync.Once
	ctr      *tcmssql.MSSQLServerContainer
	adminDSN string
	err      error
}

func rtEnsureMSSQL(t *testing.T) {
	t.Helper()
	rtMSSQL.once.Do(func() {
		ctx := context.Background()
		var err error
		rtMSSQL.ctr, err = tcmssql.Run(ctx,
			"mcr.microsoft.com/azure-sql-edge:latest",
			tcmssql.WithAcceptEULA(),
			tcmssql.WithPassword("Str0ng@Passw0rd"),
		)
		if err != nil {
			rtMSSQL.err = err
			return
		}
		rtMSSQL.adminDSN, rtMSSQL.err = rtMSSQL.ctr.ConnectionString(ctx,
			"encrypt=disable", "TrustServerCertificate=true")
	})
	if rtMSSQL.err != nil {
		t.Fatalf("roundtrip mssql container: %v", rtMSSQL.err)
	}
}

func rtMS(t *testing.T) rtFreshDB {
	t.Helper()
	rtEnsureMSSQL(t)
	ctx := context.Background()
	dbName := rtNextDB("rtms")

	adminDB, err := sql.Open("sqlserver", rtMSSQL.adminDSN)
	require.NoError(t, err)
	defer adminDB.Close()

	_, err = adminDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE [%s]", dbName))
	require.NoError(t, err)
	t.Cleanup(func() {
		d, err2 := sql.Open("sqlserver", rtMSSQL.adminDSN)
		if err2 != nil {
			return
		}
		defer d.Close()
		_, _ = d.ExecContext(context.Background(),
			fmt.Sprintf("ALTER DATABASE [%s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE", dbName))
		_, _ = d.ExecContext(context.Background(),
			fmt.Sprintf("DROP DATABASE [%s]", dbName))
	})

	dbDSN := rtMSSQL.adminDSN + "&database=" + dbName
	db, err := sql.Open("sqlserver", dbDSN)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.PingContext(ctx); err == nil {
			return rtFreshDB{db: db, dsn: dbDSN}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("timed out waiting for mssql")
	return rtFreshDB{}
}

// Plugin discovery

type rtPlugin struct {
	name         string // directory name under LYEVE_PLUGIN_TREES
	tableName    string // tracking table name for test isolation
	migrationsFS fs.FS  // os.DirFS pointing at the migrations directory
}

// rtPluginTreesEnv names the directory whose subdirectories are plugin
// checkouts, each with a migrations directory of its own.
const rtPluginTreesEnv = "LYEVE_PLUGIN_TREES"

// rtSlugRe matches every run of characters a tracking table name may not hold.
var rtSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// rtModulePath returns the module path the go.mod in dir declares, or "" when
// dir has none.
func rtModulePath(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// discoverRTPlugins finds every directory under LYEVE_PLUGIN_TREES whose
// migrations directory holds at least one .up.sql file for the postgres
// dialect. A checkout of the engine is left out, because the base schema
// already applies its tree. A symlinked entry is not a directory to
// os.ReadDir, so the checkouts have to be real directories.
func discoverRTPlugins(t *testing.T) []rtPlugin {
	t.Helper()

	root := os.Getenv(rtPluginTreesEnv)
	if root == "" {
		t.Skipf("%s names no directory of plugin checkouts", rtPluginTreesEnv)
	}
	entries, err := os.ReadDir(root)
	require.NoError(t, err)

	engine, err := os.Stat(rtEngineMigrations())
	require.NoError(t, err)
	engineModule := rtModulePath(filepath.Dir(rtEngineMigrations()))
	require.NotEmpty(t, engineModule, "the engine's go.mod names no module")

	var plugins []rtPlugin
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if rtModulePath(dir) == engineModule {
			continue
		}
		migDir := filepath.Join(dir, "migrations")
		if info, err := os.Stat(migDir); err != nil || os.SameFile(info, engine) {
			continue
		}
		psqlDir := filepath.Join(migDir, "psql")
		if _, err := os.Stat(psqlDir); err != nil {
			continue
		}
		// Check for at least one .up.sql file.
		sqlFiles, _ := filepath.Glob(filepath.Join(psqlDir, "*.up.sql"))
		if len(sqlFiles) == 0 {
			continue
		}

		// Derive a unique tracking table name from the directory name.
		slug := strings.Trim(rtSlugRe.ReplaceAllString(strings.ToLower(e.Name()), "_"), "_")
		tableName := fmt.Sprintf("rt_%s_migrations", slug)

		plugins = append(plugins, rtPlugin{
			name:         e.Name(),
			tableName:    tableName,
			migrationsFS: os.DirFS(migDir),
		})
	}

	sort.Slice(plugins, func(i, j int) bool { return plugins[i].name < plugins[j].name })
	return plugins
}

// Base schema

// rtEngineMigrations locates the engine's own migration tree.
func rtEngineMigrations() string {
	_, f, _, _ := runtime.Caller(0)
	// pkg/plugin/ -> pkg/ -> the repository root
	return filepath.Join(filepath.Dir(f), "..", "..", "migrations")
}

// applyEngineSchema lays the engine's own schema down before a plugin
// migrates over it.
//
// No plugin ever migrates into an empty database in production, because the
// engine creates sys_users and the rest of its tables before any plugin
// starts. Against an empty database, every plugin that references an engine
// table would fail on its first migration and the test would skip it whole.
func applyEngineSchema(t *testing.T, fresh rtFreshDB) {
	t.Helper()
	if _, err := enginedb.Migrate(fresh.dsn, rtEngineMigrations(), nil); err != nil {
		t.Fatalf("apply engine base schema: %v", err)
	}
}

// Cross-plugin prerequisites

// rtCreateTableRe captures the name a CREATE TABLE statement creates, quoting
// and schema prefix included. rtTableName strips both.
var rtCreateTableRe = regexp.MustCompile(`(?is)create\s+table\s+(?:if\s+not\s+exists\s+)?([^\s(;]+)`)

// rtTableName reduces a table reference to its bare lowercase name, so the
// three dialects' spellings of one table compare equal.
func rtTableName(raw string) string {
	name := strings.Trim(raw, "`\"[]")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(strings.Trim(name, "`\"[]"))
}

// rtTableOwners maps each table a discovered plugin creates to that plugin.
//
// Table ownership is the dependency. A plugin's Dependencies() cannot be read
// from here, because every plugin is its own Go module and this test reads
// their migration files off disk rather than loading their code.
// The migrations answer the same question: a failure names the table it
// wanted, and this index names the plugin whose CREATE TABLE would supply it.
func rtTableOwners(t *testing.T, plugins []rtPlugin) map[string]rtPlugin {
	t.Helper()
	owners := make(map[string]rtPlugin)
	for _, p := range plugins {
		err := fs.WalkDir(p.migrationsFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".up.sql") {
				return walkErr
			}
			body, readErr := fs.ReadFile(p.migrationsFS, path)
			if readErr != nil {
				return readErr
			}
			for _, m := range rtCreateTableRe.FindAllStringSubmatch(string(body), -1) {
				name := rtTableName(m[1])
				if _, taken := owners[name]; !taken && name != "" {
					owners[name] = p
				}
			}
			return nil
		})
		require.NoError(t, err, "index tables created by %s", p.name)
	}
	return owners
}

// rtMissingTableRes name the table in each engine's way of saying a table is
// not there. A migration that fails for any other reason matches none of them
// and is reported as the failure it is.
var rtMissingTableRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)relation "([^"]+)" does not exist`),
	regexp.MustCompile(`(?i)references invalid table '([^']+)'`),
	regexp.MustCompile(`(?i)failed to open the referenced table '([^']+)'`),
	regexp.MustCompile(`(?i)table '([^']+)' doesn't exist`),
	regexp.MustCompile(`(?i)cannot find the object "([^"]+)"`),
	regexp.MustCompile(`(?i)invalid object name '([^']+)'`),
}

// rtMissingTable returns the table a migration failure names, if it names one.
func rtMissingTable(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	for _, re := range rtMissingTableRes {
		if m := re.FindStringSubmatch(msg); m != nil {
			return rtTableName(m[1]), true
		}
	}
	return "", false
}

// rtRequireBefore puts owner ahead of position idx in list, moving it if it is
// already there and further back. It reports whether the list changed, which
// is what stops the caller looping on a prerequisite it cannot satisfy.
//
// Order matters because prerequisites have prerequisites: a view can read
// several plugins' tables, and one of those plugins can read another's.
func rtRequireBefore(list []rtPlugin, idx int, owner rtPlugin) ([]rtPlugin, bool) {
	for i, p := range list {
		if p.name == owner.name {
			if i < idx {
				return list, false
			}
			break
		}
	}
	out := make([]rtPlugin, 0, len(list)+1)
	for i, p := range list {
		if i == idx {
			out = append(out, owner)
		}
		if p.name != owner.name {
			out = append(out, p)
		}
	}
	if idx >= len(list) {
		out = append(out, owner)
	}
	return out, true
}

// rtPluginNames joins a prerequisite list for a log line.
func rtPluginNames(list []rtPlugin) string {
	names := make([]string, 0, len(list))
	for _, p := range list {
		names = append(names, p.name)
	}
	return strings.Join(names, ", ")
}

// rtNewTables returns the tables present after a step that were not present
// before it.
func rtNewTables(before, after map[string]bool) map[string]bool {
	created := make(map[string]bool)
	for name := range after {
		if !before[name] {
			created[name] = true
		}
	}
	return created
}

// rtMaxPrerequisiteRounds bounds how many clean databases one subtest will
// spend resolving prerequisites. Each round either names a plugin the previous
// round did not or moves one earlier in the order, so the bound only has to
// exceed the longest chain of prerequisites.
const rtMaxPrerequisiteRounds = 12

// Dialect table

type rtDialect struct {
	name    string
	dialect string
	fresh   func(*testing.T) rtFreshDB
}

func rtDialects(t *testing.T) []rtDialect {
	t.Helper()
	cases := []rtDialect{
		{"postgres", "postgres", rtPG},
		{"mysql", "mysql", rtMy},
		{"mssql", "mssql", rtMS},
	}
	if testing.Short() {
		return cases[:1] // postgres only in short mode
	}
	return cases
}

// Schema introspection helpers

// snapshotUserTables returns the set of non-system table names in the current
// database. The tracking table (rt_*) is excluded.
func snapshotUserTables(t *testing.T, ctx context.Context, db *sql.DB, dialect, excludeTable string) map[string]bool {
	t.Helper()
	tables := make(map[string]bool)

	switch dialect {
	case "postgres":
		rows, err := db.QueryContext(ctx,
			"SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name")
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			if name != excludeTable {
				tables[name] = true
			}
		}
		require.NoError(t, rows.Err())

	case "mysql":
		rows, err := db.QueryContext(ctx,
			"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY table_name")
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			if name != excludeTable {
				tables[name] = true
			}
		}
		require.NoError(t, rows.Err())

	case "mssql":
		rows, err := db.QueryContext(ctx,
			"SELECT name FROM sys.tables ORDER BY name")
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			if name != excludeTable {
				tables[name] = true
			}
		}
		require.NoError(t, rows.Err())
	}
	return tables
}

// countTrackingVersions returns the number of versions recorded in the
// tracking table.
func countTrackingVersions(t *testing.T, ctx context.Context, db *sql.DB, tableName string) int {
	t.Helper()
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tableName).Scan(&count)
	require.NoError(t, err)
	return count
}

// trackingTableExists returns true if the given table exists.
func trackingTableExists(t *testing.T, ctx context.Context, db *sql.DB, dialect, tableName string) bool {
	t.Helper()
	var query string
	switch dialect {
	case "postgres":
		query = "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = $1)"
	case "mysql":
		query = "SELECT COUNT(*) > 0 FROM information_schema.tables WHERE table_name = ? AND table_schema = DATABASE()"
	case "mssql":
		query = "SELECT COUNT(*) FROM sys.tables WHERE name = @p1"
	}
	var exists bool
	err := db.QueryRowContext(ctx, query, tableName).Scan(&exists)
	require.NoError(t, err)
	return exists
}

// The main roundtrip test

// TestPluginMigrationRoundtrip verifies that for every plugin with migrations:
//  1. All .up.sql scripts apply cleanly
//  2. User tables are created
//  3. All migrations can be rolled back via PluginMigrateRollback
//  4. After rollback, user tables are dropped and the tracking table is empty
//
// This catches broken .down.sql files that would cause production rollback failures.
//
// Each subtest starts from the engine's own schema and then applies whichever
// plugins own the tables this plugin's migrations reference, which is what a
// real install looks like when the plugin starts. Tables are compared against
// that starting point rather than against an empty database, so the assertions
// still describe only what the plugin under test created.
func TestPluginMigrationRoundtrip(t *testing.T) {
	plugins := discoverRTPlugins(t)
	require.NotEmpty(t, plugins, "no plugins with migrations found")
	t.Logf("discovered %d plugins with migrations", len(plugins))

	owners := rtTableOwners(t, plugins)
	dialects := rtDialects(t)

	for _, d := range dialects {
		d := d // capture
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()

			for _, p := range plugins {
				p := p // capture
				t.Run(p.name, func(t *testing.T) {
					ctx := context.Background()

					// Step 1: Apply the base schema, then all up migrations.
					//
					// A migration that fails for want of a table another
					// plugin owns adds that plugin to the prerequisites and
					// starts over on a clean database. Retrying in place is
					// not safe: MySQL commits DDL implicitly, so the version
					// the failed script had already claimed survives its
					// rollback and the retry would skip the very script that
					// failed.
					var (
						prereqs  []rtPlugin
						fresh    rtFreshDB
						baseline map[string]bool
					)
					for round := 0; ; round++ {
						require.Less(t, round, rtMaxPrerequisiteRounds,
							"prerequisite resolution for %s did not settle: %s",
							p.name, rtPluginNames(prereqs))

						fresh = d.fresh(t)
						applyEngineSchema(t, fresh)

						// resolve names the plugin that owns a table the
						// failed migration asked for and puts it at idx in the
						// order. It skips the subtest when the table belongs
						// to no plugin, or when the order already had it.
						resolve := func(idx int, err error) {
							missing, named := rtMissingTable(err)
							if !named {
								require.NoError(t, err, "PluginMigrate should succeed")
							}
							owner, known := owners[missing]
							if !known || owner.name == p.name {
								t.Skipf("skipping %s: migration wants table %q, which no plugin with migrations creates: %v",
									p.name, missing, err)
							}
							var moved bool
							prereqs, moved = rtRequireBefore(prereqs, idx, owner)
							if !moved {
								t.Skipf("skipping %s: migration still wants table %q with %s already applied: %v",
									p.name, missing, rtPluginNames(prereqs), err)
							}
						}

						settled := true
						for i, q := range prereqs {
							if err := core.PluginMigrate(ctx, fresh.db, d.dialect, q.migrationsFS, q.tableName); err != nil {
								resolve(i, err)
								settled = false
								break
							}
						}
						if !settled {
							continue
						}

						baseline = snapshotUserTables(t, ctx, fresh.db, d.dialect, p.tableName)

						err := core.PluginMigrate(ctx, fresh.db, d.dialect, p.migrationsFS, p.tableName)
						if err == nil {
							break
						}
						resolve(len(prereqs), err)
					}
					db := fresh.db

					// Step 2: Verify tracking table has entries
					applied, err := core.PluginAppliedVersions(ctx, db, p.tableName)
					require.NoError(t, err)
					assert.NotEmpty(t, applied, "at least one version should be recorded")
					versionCount := len(applied)

					// Step 3: The tables this plugin added to the base.
					created := rtNewTables(baseline, snapshotUserTables(t, ctx, db, d.dialect, p.tableName))
					assert.NotEmpty(t, created,
						"plugin %s should have created at least one user table", p.name)

					if len(prereqs) > 0 {
						t.Logf("  %s: %d versions applied, %d user tables created, after %s",
							p.name, versionCount, len(created), rtPluginNames(prereqs))
					} else {
						t.Logf("  %s: %d versions applied, %d user tables created",
							p.name, versionCount, len(created))
					}

					// Step 4: Roll back all migrations
					err = core.PluginMigrateRollback(ctx, db, d.dialect, p.migrationsFS, p.tableName, versionCount)
					require.NoError(t, err, "PluginMigrateRollback should succeed for all %d versions", versionCount)

					// Step 5: Verify tracking table is empty
					remaining := countTrackingVersions(t, ctx, db, p.tableName)
					assert.Equal(t, 0, remaining,
						"tracking table should be empty after full rollback")

					// Step 6: Verify the plugin's own tables are gone and
					// that nothing it did not create went with them.
					tablesAfterDown := snapshotUserTables(t, ctx, db, d.dialect, p.tableName)
					for table := range created {
						assert.False(t, tablesAfterDown[table],
							"table %s should have been dropped by rollback", table)
					}
					for table := range baseline {
						assert.True(t, tablesAfterDown[table],
							"table %s belongs to the engine or a prerequisite and should have survived the rollback", table)
					}

					// Step 7: Re-apply (idempotency after rollback)
					err = core.PluginMigrate(ctx, db, d.dialect, p.migrationsFS, p.tableName)
					require.NoError(t, err, "re-applying after rollback should succeed")

					reapplied, err := core.PluginAppliedVersions(ctx, db, p.tableName)
					require.NoError(t, err)
					assert.Equal(t, versionCount, len(reapplied),
						"re-apply should record same number of versions")

					// Verify tables are back.
					tablesAfterReapply := snapshotUserTables(t, ctx, db, d.dialect, p.tableName)
					for table := range created {
						assert.True(t, tablesAfterReapply[table],
							"table %s should exist after re-apply", table)
					}
				})
			}
		})
	}
}

// rtFixtureMigrations is a plugin's migration tree kept with this test. Each
// of its versions creates one table, so a rollback can be checked table by
// table.
func rtFixtureMigrations() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "testdata", "widgets", "migrations")
}

// rtFixtureTables is the table each fixture version creates, in version order.
var rtFixtureTables = []string{"example_widgets", "example_widget_parts", "example_widget_tags"}

// TestPluginMigrationRollbackPartial verifies that rolling back a subset of
// migrations works correctly: only the rolled-back versions' schema changes
// are reversed, while earlier versions remain intact.
func TestPluginMigrationRollbackPartial(t *testing.T) {
	pluginFS := os.DirFS(rtFixtureMigrations())
	tableName := "rt_partial_widgets_migrations"

	for _, d := range rtDialects(t) {
		d := d
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			fresh := d.fresh(t)
			applyEngineSchema(t, fresh)
			db := fresh.db
			ctx := context.Background()

			// Apply all.
			err := core.PluginMigrate(ctx, db, d.dialect, pluginFS, tableName)
			require.NoError(t, err)

			applied, err := core.PluginAppliedVersions(ctx, db, tableName)
			require.NoError(t, err)
			totalVersions := len(applied)
			require.Equal(t, len(rtFixtureTables), totalVersions, "every fixture version should be recorded")

			tablesBefore := snapshotUserTables(t, ctx, db, d.dialect, tableName)
			for _, table := range rtFixtureTables {
				require.True(t, tablesBefore[table], "table %s should exist after migrating", table)
			}

			// Roll back only the last version.
			err = core.PluginMigrateRollback(ctx, db, d.dialect, pluginFS, tableName, 1)
			require.NoError(t, err, "rolling back 1 version should succeed")

			afterPartial, err := core.PluginAppliedVersions(ctx, db, tableName)
			require.NoError(t, err)
			assert.Equal(t, totalVersions-1, len(afterPartial),
				"one fewer version should be recorded")

			// The last version's table is gone, and every earlier one remains.
			last := len(rtFixtureTables) - 1
			tablesAfterPartial := snapshotUserTables(t, ctx, db, d.dialect, tableName)
			for i, table := range rtFixtureTables {
				if i == last {
					assert.False(t, tablesAfterPartial[table], "table %s should have been dropped by the partial rollback", table)
					continue
				}
				assert.True(t, tablesAfterPartial[table], "table %s should have survived the partial rollback", table)
			}

			// Now roll back the rest.
			remaining := totalVersions - 1
			err = core.PluginMigrateRollback(ctx, db, d.dialect, pluginFS, tableName, remaining)
			require.NoError(t, err, "rolling back remaining %d versions should succeed", remaining)

			final := countTrackingVersions(t, ctx, db, tableName)
			assert.Equal(t, 0, final, "tracking table should be empty after full rollback")

			tablesAfterAll := snapshotUserTables(t, ctx, db, d.dialect, tableName)
			for _, table := range rtFixtureTables {
				assert.False(t, tablesAfterAll[table], "table %s should have been dropped by the full rollback", table)
			}
		})
	}
}
