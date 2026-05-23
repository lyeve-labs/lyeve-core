// PluginMigrate reads the applied-versions table on every boot. The signature
// cache avoids this round-trip: a content-hash of migration files is stored
// after a successful run. On the next boot a matching hash skips the DB query.
// Stored in _lyeve_migration_signatures, keyed by migration tracking table
// name. Concurrent-safe: the cache read is a single-row SELECT under the
// database's own transaction isolation.

package plugin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// MigrationSigCache provides a fast-path migration check by caching content
// hashes of migration files. When the stored hash matches the current set of
// files, PluginMigrate skips the applied-versions query entirely.
//
// Create one instance per process. Thread-safe (the underlying DB handles
// concurrent reads/writes to the signatures table).
type MigrationSigCache struct {
	db      *sql.DB
	dialect string // set by Init. Used for dialect-appropriate placeholders
	ph      string // placeholder for single-param queries ($1, ?, @p1)
}

// NewMigrationSigCache creates a cache backed by the given database.
// db must be non-nil.
func NewMigrationSigCache(db *sql.DB) *MigrationSigCache {
	return &MigrationSigCache{db: db}
}

// Init creates the signatures table if it doesn't exist. Idempotent. Safe
// to call on every boot. Uses dialect-appropriate DDL.
func (c *MigrationSigCache) Init(ctx context.Context, dialect string) error {
	var ddl string
	switch dialect {
	case "mysql":
		ddl = "CREATE TABLE IF NOT EXISTS _lyeve_migration_signatures (table_name VARCHAR(255) NOT NULL PRIMARY KEY, sig_hash VARCHAR(128) NOT NULL, updated_at DATETIME(6) NOT NULL DEFAULT NOW(6)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"
		c.ph = "?"
	case "mssql":
		ddl = "IF OBJECT_ID(N'_lyeve_migration_signatures', N'U') IS NULL CREATE TABLE _lyeve_migration_signatures (table_name NVARCHAR(255) NOT NULL PRIMARY KEY, sig_hash NVARCHAR(128) NOT NULL, updated_at DATETIME2(7) NOT NULL DEFAULT SYSUTCDATETIME())"
		c.ph = "@p1"
	default:
		ddl = "CREATE TABLE IF NOT EXISTS _lyeve_migration_signatures (table_name TEXT PRIMARY KEY, sig_hash TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())"
		c.ph = "$1"
	}
	c.dialect = dialect
	_, err := c.db.ExecContext(ctx, ddl)
	if err != nil {
		return fmt.Errorf("migration sig cache init: %w", err)
	}
	return nil
}

// NeedsMigration returns true when the migration files have changed since the
// last successful migration run (or when they have never run). When false,
// the caller can skip the full migration check entirely.
//
// tableName is the plugin's migration tracking table (e.g.
// "plugin_content_schema_migrations"). migrationsFS is the embedded FS
// containing psql/mysql/mssql/*.up.sql files.
func (c *MigrationSigCache) NeedsMigration(ctx context.Context, tableName string, migrationsFS fs.FS) (bool, error) {
	if c == nil || c.db == nil {
		return true, nil // no cache -> always check
	}

	newSig, err := computeMigrationSig(migrationsFS)
	if err != nil {
		return true, fmt.Errorf("migration sig cache: compute: %w", err)
	}

	stored, err := c.getStored(ctx, tableName)
	if err != nil {
		return true, err
	}

	// No stored signature -> first boot, needs migration.
	if stored == "" {
		return true, nil
	}

	return stored != newSig, nil
}

// Record stores the signature after a successful migration run.
func (c *MigrationSigCache) Record(ctx context.Context, dialect, tableName string, migrationsFS fs.FS) error {
	if c == nil || c.db == nil {
		return nil
	}

	sig, err := computeMigrationSig(migrationsFS)
	if err != nil {
		return fmt.Errorf("migration sig cache: compute for record: %w", err)
	}

	var upsertSQL string
	switch dialect {
	case "mysql":
		upsertSQL = "INSERT INTO _lyeve_migration_signatures (table_name, sig_hash) VALUES (?, ?) ON DUPLICATE KEY UPDATE sig_hash = ?, updated_at = NOW(6)"
		_, err = c.db.ExecContext(ctx, upsertSQL, tableName, sig, sig)
	case "mssql":
		// The trailing semicolon is required, not stylistic: MSSQL rejects an
		// unterminated MERGE outright, so without it every signature record
		// would fail and the cache would never populate on MSSQL.
		upsertSQL = "MERGE INTO _lyeve_migration_signatures WITH (HOLDLOCK) AS t USING (VALUES (@p1, @p2)) AS s(table_name, sig_hash) ON t.table_name = s.table_name WHEN MATCHED THEN UPDATE SET sig_hash = @p2, updated_at = SYSUTCDATETIME() WHEN NOT MATCHED THEN INSERT (table_name, sig_hash) VALUES (@p1, @p2);"
		_, err = c.db.ExecContext(ctx, upsertSQL, tableName, sig)
	default:
		upsertSQL = "INSERT INTO _lyeve_migration_signatures (table_name, sig_hash) VALUES ($1, $2) ON CONFLICT (table_name) DO UPDATE SET sig_hash = $3, updated_at = NOW()"
		_, err = c.db.ExecContext(ctx, upsertSQL, tableName, sig, sig)
	}
	if err != nil {
		return fmt.Errorf("migration sig cache: record: %w", err)
	}
	return nil
}

// Invalidate removes a signature entry, forcing a full migration check on the
// next boot. Useful for manual migration repair operations.
func (c *MigrationSigCache) Invalidate(ctx context.Context, tableName string) error {
	if c == nil || c.db == nil {
		return nil
	}
	ph := c.ph
	if ph == "" {
		ph = "$1"
	}
	_, err := c.db.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM _lyeve_migration_signatures WHERE table_name = %s", ph),
		tableName,
	)
	return err
}

func (c *MigrationSigCache) getStored(ctx context.Context, tableName string) (string, error) {
	var hash string
	ph := c.ph
	if ph == "" {
		ph = "$1" // defensive: Init not called
	}
	err := c.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT sig_hash FROM _lyeve_migration_signatures WHERE table_name = %s", ph),
		tableName,
	).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("migration sig cache: lookup %s: %w", tableName, err)
	}
	return hash, nil
}

// computeMigrationSig hashes the names and contents of all *.up.sql files
// across ALL dialect subdirectories, sorted deterministically. This ensures
// the cache invalidates when any migration script is added, removed, or
// modified in any dialect.
func computeMigrationSig(migrationsFS fs.FS) (string, error) {
	type entry struct {
		name    string
		content string
	}
	var entries []entry

	dialects := []string{"psql", "mysql", "mssql"}
	for _, dir := range dialects {
		fileEntries, err := fs.ReadDir(migrationsFS, dir)
		if err != nil {
			continue // missing dialect dir is not an error
		}
		for _, fe := range fileEntries {
			if fe.IsDir() || !strings.HasSuffix(fe.Name(), ".up.sql") {
				continue
			}
			body, err := fs.ReadFile(migrationsFS, dir+"/"+fe.Name())
			if err != nil {
				return "", fmt.Errorf("read %s/%s: %w", dir, fe.Name(), err)
			}
			entries = append(entries, entry{
				name:    dir + "/" + fe.Name(),
				content: string(body),
			})
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.name))
		h.Write([]byte{0})
		h.Write([]byte(e.content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
