package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// PluginMigrateRollback rolls back the last N migration versions by running
// the corresponding .down.sql scripts in reverse order. Each script runs in
// its own transaction. After each successful rollback, the version is removed
// from the bookkeeping table.
//
// If db is nil the call is a no-op.
func PluginMigrateRollback(ctx context.Context, db *sql.DB, dialect string, migrationsFS fs.FS, tableName string, n int) error {
	if db == nil {
		return nil
	}
	if n <= 0 {
		return nil
	}

	dir := dialectDir(dialect)

	// Load all down scripts.
	scripts, err := loadDownScripts(migrationsFS, dir)
	if err != nil {
		return fmt.Errorf("plugin migrate rollback: load down scripts from %s: %w", dir, err)
	}
	if len(scripts) == 0 {
		return fmt.Errorf("plugin migrate rollback: no down scripts found in %s", dir)
	}

	// Get applied versions.
	applied, err := appliedVersions(ctx, db, tableName)
	if err != nil {
		return fmt.Errorf("plugin migrate rollback: read applied versions: %w", err)
	}

	// Determine which versions to roll back (last N).
	var appliedSorted []string
	for v := range applied {
		appliedSorted = append(appliedSorted, v)
	}
	sort.Strings(appliedSorted)

	if len(appliedSorted) == 0 {
		return nil // nothing to roll back
	}

	toRollback := appliedSorted
	if len(toRollback) > n {
		toRollback = toRollback[len(toRollback)-n:]
	}

	// Roll back in reverse order.
	for i := len(toRollback) - 1; i >= 0; i-- {
		v := toRollback[i]
		sc, ok := scripts[v]
		if !ok {
			return fmt.Errorf("plugin migrate rollback: no down script for version %s", v)
		}
		// Defense-in-depth: a plugin's rollback may drop its own sys_-prefixed
		// tables (the sys_ prefix is a plugin convention), but never a table the
		// engine owns, which compliance.CheckCoreSysTableDDL refuses.
		if err := compliance.CheckCoreSysTableDDL(sc.sql); err != nil {
			return fmt.Errorf("plugin migrate rollback: %s: %w", v, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("plugin migrate rollback: begin tx for %s: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx, sc.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("plugin migrate rollback: exec down %s: %w", v, err)
		}
		// Remove from bookkeeping table.
		deleteSQL := fmt.Sprintf("DELETE FROM %s WHERE version = %s", tableName, placeholderFor(dialect))
		if _, err := tx.ExecContext(ctx, deleteSQL, v); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("plugin migrate rollback: delete version %s: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("plugin migrate rollback: commit %s: %w", v, err)
		}
	}
	return nil
}

// PluginAppliedVersions returns the set of migration versions recorded in the
// bookkeeping table. Returns an empty map if the table doesn't exist yet.
func PluginAppliedVersions(ctx context.Context, db *sql.DB, tableName string) (map[string]bool, error) {
	if db == nil {
		return map[string]bool{}, nil
	}
	return appliedVersions(ctx, db, tableName)
}

// placeholderFor returns the dialect-appropriate placeholder for a single value.
func placeholderFor(dialect string) string {
	switch dialect {
	case "mysql":
		return "?"
	case "mssql":
		return "@p1"
	default:
		return "$1"
	}
}

// loadDownScripts reads every *.down.sql file from the named subdirectory and
// returns them keyed by version.
func loadDownScripts(efs fs.FS, subdir string) (map[string]migrationScript, error) {
	entries, err := fs.ReadDir(efs, subdir)
	if err != nil {
		// Treat a missing dialect directory as "no migrations yet", not an error.
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]migrationScript{}, nil
		}
		return nil, err
	}
	out := make(map[string]migrationScript)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".down.sql") {
			continue
		}
		body, err := fs.ReadFile(efs, subdir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		version := strings.TrimSuffix(name, ".down.sql")
		out[version] = migrationScript{
			version: version,
			sql:     string(body),
		}
	}
	return out, nil
}
