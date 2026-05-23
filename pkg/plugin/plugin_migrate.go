package plugin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"

	"log/slog"
)

// migrationsTableNameRe restricts migration tracking table names to lowercase
// ASCII letters, digits, and underscores: the same alphabet as plugin IDs.
// This prevents a future dynamic/derived tableName from silently injecting
// into the CREATE TABLE / INSERT / SELECT statements that build SQL via
// fmt.Sprintf with no quoting layer.
var migrationsTableNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// migrationSigCache is the process-wide signature cache for lazy migration
// checks. When set, PluginMigrate consults it before querying the applied
// versions table: if migration files haven't changed since last boot, the
// entire DB round-trip is skipped. Set by the runtime via SetMigrationSigCache
// after the database pool is connected. Nil means no caching (always check).
var migrationSigCache *MigrationSigCache

// SetMigrationSigCache wires the migration signature cache for the process.
// Call once during boot, after the database connection is established.
// Passing nil disables the cache.
func SetMigrationSigCache(cache *MigrationSigCache) {
	migrationSigCache = cache
}

// PluginMigrate applies pending *.up.sql migration scripts for the given
// database dialect from an embedded filesystem organized like:
//
//	migrations/psql/*.up.sql    (PostgreSQL)
//	migrations/mysql/*.up.sql   (MySQL)
//	migrations/mssql/*.up.sql   (MSSQL)
//
// Scripts are applied in lexicographic order, one per transaction. A
// bookkeeping table (given by tableName) tracks which versions have been
// applied so that the runner is idempotent.
//
// When a MigrationSigCache is wired (via SetMigrationSigCache), PluginMigrate
// first checks whether the migration files have changed since the last
// successful run. On cache hit, the applied versions query is skipped
// entirely: plugins that haven't changed their migrations avoid a DB
// round-trip on every boot.
//
// If db is nil the call is a no-op.
//
// The function is safe to call from any plugin's Start. It serializes
// concurrent boots via the database's own transaction isolation.
func PluginMigrate(ctx context.Context, db *sql.DB, dialect string, migrationsFS fs.FS, tableName string) error {
	// Validate tableName
	// The name is interpolated into CREATE TABLE / INSERT / SELECT
	// statements via fmt.Sprintf with no quoting layer. Every caller
	// passes a compile-time constant, so there is no attacker path.
	// This guard keeps a dynamic or derived name from injecting.
	if !migrationsTableNameRe.MatchString(tableName) {
		return fmt.Errorf("plugin migrate: invalid table name %q: must match %s",
			tableName, migrationsTableNameRe.String())
	}

	if db == nil {
		return nil
	}

	// Lazy migration check
	// When the migration signature cache is wired, check whether the
	// migration files have changed since the last successful run. A
	// cache hit skips the applied-versions DB query entirely.
	cache := migrationSigCache
	if cache != nil {
		needs, err := cache.NeedsMigration(ctx, tableName, migrationsFS)
		if err != nil {
			slog.Warn("plugin migrate: signature cache check failed - falling back to full check",
				"plugin_table", tableName, "err", err)
		} else if !needs {
			return nil // no migrations changed since last boot
		}
	}

	dir := dialectDir(dialect)

	// Load scripts from the dialect-specific subdirectory
	scripts, err := loadUpScripts(migrationsFS, dir)
	if err != nil {
		return fmt.Errorf("plugin migrate: load scripts from %s: %w", dir, err)
	}
	if len(scripts) == 0 {
		return nil // nothing to run (e.g. empty mysql/ dir on a new dialect)
	}

	// Everything that touches schema runs under a cross-instance lock
	// Two things need it when several replicas start at once.
	//
	// CREATE TABLE IF NOT EXISTS is not atomic against a concurrent identical
	// create on Postgres: both sessions pass the existence check and the loser
	// fails on a pg_type unique violation, so the plugin does not start.
	//
	// The apply loop then claims a version with a deduplicating INSERT and lets
	// the loser skip it, which is only safe while the claim and the DDL commit
	// together. That holds on Postgres and not on MySQL, where DDL implicitly
	// commits: a loser could see version 001 claimed, skip it, and run 002
	// against a table the winner had not finished creating.
	unlock, err := acquireMigrationLock(ctx, db, dialect)
	if err != nil {
		return err
	}
	defer unlock()

	// Ensure the bookkeeping table exists
	createSQL := migrationsTableSQL(tableName, dialect)
	if _, err := db.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("plugin migrate: create tracking table %s: %w", tableName, err)
	}
	if err := ensureTrackingColumns(ctx, db, dialect, tableName); err != nil {
		return err
	}

	// Determine which versions are already applied
	// Read under the lock: a holder that finished while this caller waited has
	// already applied everything, and a stale snapshot would re-run it.
	recorded, err := recordedVersions(ctx, db, tableName)
	if err != nil {
		return fmt.Errorf("plugin migrate: read applied versions: %w", err)
	}
	if err := checkRecorded(tableName, scripts, recorded); err != nil {
		return err
	}

	for _, sc := range scripts {
		if _, done := recorded[sc.version]; done {
			continue
		}
		if err := applyScriptWithRetry(ctx, db, dialect, tableName, sc); err != nil {
			return err
		}
	}
	// Record migration signature after successful run
	// Cache the content hash so the next boot can skip the applied-versions
	// query when migration files haven't changed.
	if cache != nil {
		if err := cache.Record(ctx, dialect, tableName, migrationsFS); err != nil {
			slog.Warn("plugin migrate: signature cache record failed",
				"plugin_table", tableName, "err", err)
		}
	}
	return nil
}

// maxMigrationAttempts bounds retries of a single migration script.
const maxMigrationAttempts = 4

// applyScriptWithRetry applies one script, retrying transient concurrency
// failures.
//
// Plugins migrate concurrently at boot and several touch the same catalog
// tables, so InnoDB picks a deadlock victim and fails it. MySQL classes this as
// the caller's to retry ("try restarting transaction"), and without the retry
// the affected plugin simply does not start. Backoff is linear and short: the
// contention lasts as long as the other plugin's transaction, not longer.
func applyScriptWithRetry(ctx context.Context, db *sql.DB, dialect, tableName string, sc migrationScript) error {
	var lastErr error
	for attempt := 1; attempt <= maxMigrationAttempts; attempt++ {
		err := applyScript(ctx, db, dialect, tableName, sc)
		if err == nil {
			return nil
		}
		if !isRetryableTxError(err) {
			return err
		}
		lastErr = err
		slog.Warn("plugin migrate: transient conflict, retrying",
			"plugin_table", tableName, "version", sc.version,
			"attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
		}
	}
	return fmt.Errorf("plugin migrate: %s failed after %d attempts: %w",
		sc.version, maxMigrationAttempts, lastErr)
}

// isRetryableTxError reports whether err is a transient concurrency failure the
// database expects the client to retry. The classification lives in pkg/sqlx so
// plugin stores hitting the same deadlocks use the same definition.
func isRetryableTxError(err error) bool { return sqlx.IsRetryableTxError(err) }

// applyScript claims and runs a single migration script in one transaction.
//
// The claim is written incomplete and marked complete, with the script's
// checksum, only after the script returns. On PostgreSQL and SQL Server the
// three statements commit together, so the mark changes nothing there. MySQL
// commits DDL implicitly, so the first CREATE in a script makes the claim
// durable and a later statement can still fail. The claim then outlives the
// script and stays incomplete, and the next boot refuses to start on it rather
// than skipping a version that only half ran.
func applyScript(ctx context.Context, db *sql.DB, dialect, tableName string, sc migrationScript) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("plugin migrate: begin tx for %s: %w", sc.version, err)
	}
	// Try to claim this version
	// The INSERT dedup (per dialect) silently skips if another boot
	// already recorded this version.
	insertSQL, insertArgs := insertVersionStmt(tableName, dialect, sc.version)
	res, err := tx.ExecContext(ctx, insertSQL, insertArgs...)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("plugin migrate: record %s: %w", sc.version, err)
	}
	rows, raErr := res.RowsAffected()
	if raErr == nil && rows == 0 {
		// Another concurrent boot already claimed this version.
		// Commit the empty transaction and move on.
		_ = tx.Commit()
		return nil
	}
	// The winner proceeds: run the migration SQL.
	// If RowsAffected returned an error (driver support gap), the
	// caller optimistically runs the migration. INSERT dedup prevents
	// crashes if another boot runs the same SQL in parallel.
	if _, err := tx.ExecContext(ctx, sc.sql); err != nil {
		_ = tx.Rollback()
		if sqlx.IsDuplicateObject(err) {
			return fmt.Errorf("plugin migrate: exec %s: %w%s", sc.version, err,
				lostRecordHint(tableName, dialect, sc.version))
		}
		return fmt.Errorf("plugin migrate: exec %s: %w", sc.version, err)
	}
	if _, err := tx.ExecContext(ctx, completeVersionStmt(tableName, dialect), scriptChecksum(sc.sql), sc.version); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("plugin migrate: mark %s complete: %w", sc.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("plugin migrate: commit %s: %w", sc.version, err)
	}
	return nil
}

// lostRecordHint explains a migration that failed because the schema already
// holds what the script creates.
//
// The engine reaches this line only after claiming a version the bookkeeping
// table had no record of, so it knows two things at once: this version is
// unrecorded, and the object it creates is already there. That pair is one of
// exactly two situations and the engine cannot tell them apart. Either the
// migration is wrong, creating something an earlier migration in the same tree
// already created, or the install lost its bookkeeping row while keeping the
// schema, through a hand repair, a partial restore or a claim that rolled back
// with a failed script.
//
// Guessing is worse than saying so. Treating the error as success would record
// a version whose script may have failed halfway for an unrelated reason, and
// requiring every dialect to write idempotent DDL is not possible, because
// MySQL and SQL Server have no ADD COLUMN IF NOT EXISTS.
//
// So the engine fails and names the repair, and the repair depends on the
// engine. PostgreSQL never gets here, because a plugin can write ADD COLUMN IF
// NOT EXISTS and the re-apply is a no-op. SQL Server rolls the claim back with
// the script, so the table holds no row for the version and an INSERT records
// it. Without that row every boot repeats the same failure. MySQL commits DDL
// implicitly, so the claim outlives the failed script as a row marked
// incomplete. An INSERT would collide with that row, and the next boot stops
// on it, so the repair there marks the existing row complete.
func lostRecordHint(tableName, dialect, version string) string {
	repair := fmt.Sprintf("INSERT INTO %s (version) VALUES ('%s')", tableName, version)
	if dialect == "mysql" {
		repair = fmt.Sprintf("UPDATE %s SET completed = 1 WHERE version = '%s'", tableName, version)
	}
	return fmt.Sprintf(
		" (the schema already holds what this migration creates."+
			" Either the migration is wrong, or this install lost its record of it."+
			" After confirming the schema already has this change, record it with: %s)",
		repair)
}

// helpers

// dialectDir maps a dialect name to the migration subdirectory convention
// used by the core engine (psql, mysql, mssql).
func dialectDir(dialect string) string {
	switch dialect {
	case "mysql":
		return "mysql"
	case "mssql":
		return "mssql"
	default:
		return "psql"
	}
}

// migrationsTableSQL returns the CREATE TABLE statement for the plugin's
// migrations bookkeeping table, fully per-dialect. MySQL uses VARCHAR for PK,
// MSSQL uses NVARCHAR and OBJECT_ID guard, PostgreSQL uses TEXT.
func migrationsTableSQL(tableName, dialect string) string {
	switch dialect {
	case "mysql":
		return fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s (version VARCHAR(255) NOT NULL PRIMARY KEY, applied_at DATETIME(6) NOT NULL DEFAULT NOW(6)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
			tableName,
		)
	case "mssql":
		return fmt.Sprintf(
			"IF OBJECT_ID(N'%s', N'U') IS NULL CREATE TABLE %s (version NVARCHAR(255) NOT NULL PRIMARY KEY, applied_at DATETIME2(7) NOT NULL DEFAULT SYSUTCDATETIME())",
			tableName, tableName,
		)
	default:
		return fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())",
			tableName,
		)
	}
}

// insertVersionStmt returns the INSERT that claims a version, with the
// dialect-appropriate placeholders and the arguments that go with it.
//
// Zero affected rows has to mean one thing and one thing only: another boot
// already recorded this version. applyScript reads it that way and skips the
// migration, so any other statement that can report zero rows makes the
// migrator skip work it never did and report success.
//
// That rules out INSERT IGNORE on MySQL, because IGNORE downgrades every error
// to a warning: a lock-wait timeout or a deadlock against a boot racing for the
// same version would report zero rows and no error. Both boots would then
// conclude the other one won, both would skip, and the migration would never
// be applied on either. The plugin would start, say nothing, and run against a
// schema without the table it expects.
//
// The guarded INSERT ... SELECT counts rows actually inserted and lets every
// other error surface to applyScriptWithRetry, which is what the retry is for.
// The PostgreSQL and SQL Server forms behave the same way.
func insertVersionStmt(tableName, dialect, version string) (string, []any) {
	switch dialect {
	case "mysql":
		return fmt.Sprintf(
			"INSERT INTO %s (version, completed) SELECT ?, 0 FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM %s AS existing WHERE existing.version = ?)",
			tableName, tableName,
		), []any{version, version}
	case "mssql":
		return fmt.Sprintf("MERGE %s AS t USING (VALUES (@p1)) AS s(version) ON t.version = s.version WHEN NOT MATCHED THEN INSERT (version, completed) VALUES (s.version, 0);", tableName),
			[]any{version}
	default:
		return fmt.Sprintf("INSERT INTO %s (version, completed) VALUES ($1, 0) ON CONFLICT (version) DO NOTHING", tableName),
			[]any{version}
	}
}

// completeVersionStmt returns the UPDATE that marks a claimed version applied
// and records the checksum of the script that ran. Its arguments are the
// checksum, then the version.
func completeVersionStmt(tableName, dialect string) string {
	switch dialect {
	case "mysql":
		return fmt.Sprintf("UPDATE %s SET completed = 1, checksum = ? WHERE version = ?", tableName)
	case "mssql":
		return fmt.Sprintf("UPDATE %s SET completed = 1, checksum = @p1 WHERE version = @p2", tableName)
	default:
		return fmt.Sprintf("UPDATE %s SET completed = 1, checksum = $1 WHERE version = $2", tableName)
	}
}

// scriptChecksum is the hex SHA-256 of a script's bytes, as the bookkeeping
// table records it.
func scriptChecksum(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])
}

// trackingColumns are the bookkeeping columns ensureTrackingColumns adds to a
// table that lacks them. Each default describes a row that holds no value for
// the column: such a version was recorded only after it applied, so it is
// complete, and nothing recorded what it ran, so its checksum is unknown.
var trackingColumns = []struct {
	name               string
	psql, mysql, mssql string
}{
	{"completed", "SMALLINT NOT NULL DEFAULT 1", "SMALLINT NOT NULL DEFAULT 1", "SMALLINT NOT NULL DEFAULT 1"},
	{"checksum", "VARCHAR(64) NULL", "VARCHAR(64) NULL", "VARCHAR(64) NULL"},
}

// ensureTrackingColumns adds the columns in trackingColumns to a bookkeeping
// table that lacks them. MySQL has no ADD COLUMN IF NOT EXISTS, so
// its branch asks information_schema first.
func ensureTrackingColumns(ctx context.Context, db *sql.DB, dialect, tableName string) error {
	for _, c := range trackingColumns {
		var stmt string
		switch dialect {
		case "mysql":
			var n int
			if err := db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?",
				tableName, c.name).Scan(&n); err != nil {
				return fmt.Errorf("plugin migrate: read columns of %s: %w", tableName, err)
			}
			if n > 0 {
				continue
			}
			stmt = fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, c.name, c.mysql)
		case "mssql":
			stmt = fmt.Sprintf("IF COL_LENGTH(N'%s', N'%s') IS NULL ALTER TABLE %s ADD %s %s",
				tableName, c.name, tableName, c.name, c.mssql)
		default:
			stmt = fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", tableName, c.name, c.psql)
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("plugin migrate: add column %s to %s: %w", c.name, tableName, err)
		}
	}
	return nil
}

// recordedVersion is one row of the bookkeeping table.
type recordedVersion struct {
	completed bool
	checksum  string // empty when the row carries no checksum
}

// recordedVersions reads every row of the bookkeeping table.
func recordedVersions(ctx context.Context, db *sql.DB, tableName string) (map[string]recordedVersion, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, completed, checksum FROM `+tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]recordedVersion{}
	for rows.Next() {
		var (
			v         string
			completed int
			checksum  sql.NullString
		)
		if err := rows.Scan(&v, &completed, &checksum); err != nil {
			return nil, err
		}
		out[v] = recordedVersion{completed: completed != 0, checksum: checksum.String}
	}
	return out, rows.Err()
}

// checkRecorded refuses to migrate when the bookkeeping table promises
// something the schema may not hold.
//
// An incomplete row is a claim whose script did not finish. Only MySQL leaves
// one, because only MySQL commits the claim before the script ends, and the
// schema then holds whatever the script did before it failed. Skipping the
// version would start the plugin against that schema, and running it again
// would repeat the statements that already ran, so the boot stops and names
// both repairs.
//
// A checksum that differs from the script on disk means the script was edited
// after it applied. Skipping by name would leave the database with what the
// applied text did, and the code would then fail far from the cause. A row
// without a checksum cannot be checked.
func checkRecorded(tableName string, scripts []migrationScript, recorded map[string]recordedVersion) error {
	var incomplete []string
	for v, r := range recorded {
		if !r.completed {
			incomplete = append(incomplete, v)
		}
	}
	if len(incomplete) > 0 {
		sort.Strings(incomplete)
		v := incomplete[0]
		return fmt.Errorf("plugin migrate: %s records %s as started and never completed."+
			" The script failed partway on a database that commits DDL as it goes,"+
			" so the schema holds part of it. Finish or undo the script by hand, then either"+
			" mark it applied with UPDATE %s SET completed = 1 WHERE version = '%s'"+
			" or remove the claim with DELETE FROM %s WHERE version = '%s' to run it again",
			tableName, v, tableName, v, tableName, v)
	}
	for _, sc := range scripts {
		r, ok := recorded[sc.version]
		if !ok || r.checksum == "" {
			continue
		}
		if r.checksum != scriptChecksum(sc.sql) {
			return fmt.Errorf("plugin migrate: %s records %s as applied from different contents than the script now holds."+
				" A script that already ran is never run again, so the edit never reaches this database."+
				" Restore the script and put the change in a new migration",
				tableName, sc.version)
		}
	}
	return nil
}

type migrationScript struct {
	version string
	sql     string
}

// loadUpScripts reads every *.up.sql file from the named subdirectory of
// the embedded FS and returns them sorted lexicographically (which is also
// version order because filenames are prefixed with a zero-padded sequence
// number).
func loadUpScripts(efs fs.FS, subdir string) ([]migrationScript, error) {
	entries, err := fs.ReadDir(efs, subdir)
	if err != nil {
		// Treat a missing dialect directory as "no migrations yet", not an error.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var scripts []migrationScript
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		body, err := fs.ReadFile(efs, subdir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		scripts = append(scripts, migrationScript{
			version: strings.TrimSuffix(name, ".up.sql"),
			sql:     string(body),
		})
	}
	sort.Slice(scripts, func(i, j int) bool { return scripts[i].version < scripts[j].version })
	return scripts, nil
}

// appliedVersions returns the set of migration version strings already
// recorded in the bookkeeping table.
func appliedVersions(ctx context.Context, db *sql.DB, tableName string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM `+tableName)
	if err != nil {
		// The tracking table might not exist yet: the CREATE IF NOT EXISTS
		// ran just above, so any other error type is real.
		if IsTableNotExistError(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// IsTableNotExistError checks whether err is a dialect-appropriate
// "relation/table does not exist" error, matched through each driver's native
// error type rather than message text.
//
// Plugins use it where their contract treats a missing table as an empty
// result set rather than a failure (e.g. a delete that returns false when
// nothing matched), and the content API uses it to answer a read of a deleted
// content type with 404 instead of the store default of 503.
//
// The match itself lives in sqlx so the two callers cannot drift: SQL Server
// numbers this failure differently per statement, and a list that is right in
// one place and short in the other is worse than no list.
func IsTableNotExistError(err error) bool {
	return sqlx.IsUndefinedTable(err)
}
