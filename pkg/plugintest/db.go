package plugintest

import (
	"context"
	"fmt"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// DBFixture provides database seeding and assertion helpers for plugin
// integration tests. It wraps the host's Querier to avoid raw SQL in tests.
type DBFixture struct {
	t    T
	host core.Host
}

// NewDBFixture creates a DBFixture scoped to the given host.
func NewDBFixture(t T, host core.Host) *DBFixture {
	return &DBFixture{t: t, host: host}
}

// Seed inserts a row into table. Map keys must match column names. UUIDs and
// timestamps must be provided: plugintest does not auto-generate them.
func (f *DBFixture) Seed(ctx context.Context, table string, row map[string]any) {
	f.t.Helper()
	columns, values, placeholders := buildInsert(row)
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		table, columns, placeholders)
	_, err := f.host.Querier(ctx).Exec(ctx, sql, values...)
	if err != nil {
		f.t.Fatalf("Seed %s: %v", table, err)
	}
}

// SeedMany inserts multiple rows into table. All rows must have the same column set.
func (f *DBFixture) SeedMany(ctx context.Context, table string, rows []map[string]any) {
	f.t.Helper()
	for _, row := range rows {
		f.Seed(ctx, table, row)
	}
}

// AssertCount fails if table does not contain exactly want rows.
func (f *DBFixture) AssertCount(ctx context.Context, table string, want int) {
	f.t.Helper()
	count := f.CountRows(ctx, table)
	if count != want {
		f.t.Errorf("expected %d rows in %q, got %d", want, table, count)
	}
}

// CountRows returns the row count of table.
func (f *DBFixture) CountRows(ctx context.Context, table string) int {
	f.t.Helper()
	var count int
	row, err := f.host.Querier(ctx).QueryRow(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", table))
	if err != nil {
		f.t.Fatalf("CountRows(%s): %v", table, err)
	}
	if err := row.Scan(&count); err != nil {
		f.t.Fatalf("CountRows %s: %v", table, err)
	}
	return count
}

// AssertTableExists fails if table does not exist.
func (f *DBFixture) AssertTableExists(ctx context.Context, table string) {
	f.t.Helper()
	if !f.TableExists(ctx, table) {
		f.t.Errorf("expected table %q to exist, but it does not", table)
	}
}

// AssertTableNotExists fails if table exists.
func (f *DBFixture) AssertTableNotExists(ctx context.Context, table string) {
	f.t.Helper()
	if f.TableExists(ctx, table) {
		f.t.Errorf("expected table %q NOT to exist, but it does", table)
	}
}

// TableExists reports whether table exists via information_schema.
func (f *DBFixture) TableExists(ctx context.Context, table string) bool {
	f.t.Helper()
	var count int
	// Use information_schema which is portable across PG/MySQL/MSSQL.
	row, err := f.host.Querier(ctx).QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = $1`,
		table)
	if err != nil {
		f.t.Fatalf("TableExists(%s): %v", table, err)
	}
	if err := row.Scan(&count); err != nil {
		f.t.Fatalf("TableExists %s: %v", table, err)
	}
	return count > 0
}

// Truncate removes all rows from table via TRUNCATE TABLE.
func (f *DBFixture) Truncate(ctx context.Context, table string) {
	f.t.Helper()
	_, err := f.host.Querier(ctx).Exec(ctx, fmt.Sprintf("TRUNCATE TABLE %s", table))
	if err != nil {
		f.t.Fatalf("Truncate %s: %v", table, err)
	}
}

// WithDBFixture starts the plugin, creates a DBFixture, and registers cleanup
// that truncates cleanupTables when the test ends.
func WithDBFixture(t T, ctx context.Context, host core.Host, plugin core.Plugin, cleanupTables ...string) (core.Plugin, *DBFixture) {
	t.Helper()
	if err := plugin.Start(ctx, host); err != nil {
		t.Fatalf("WithDBFixture Start: %v", err)
	}
	db := NewDBFixture(t, host)
	if tb, ok := t.(*testing.T); ok {
		tb.Cleanup(func() {
			for _, table := range cleanupTables {
				if db.TableExists(ctx, table) {
					db.Truncate(ctx, table)
				}
			}
		})
	}
	return plugin, db
}

// helpers

// buildInsert builds SQL INSERT column list, args slice, and placeholder string.
// Uses PostgreSQL $N placeholders. Other dialects are handled by the engine.
func buildInsert(row map[string]any) (columns string, args []any, placeholders string) {
	i := 0
	for col, val := range row {
		if i > 0 {
			columns += ", "
			placeholders += ", "
		}
		columns += col
		args = append(args, val)
		placeholders += fmt.Sprintf("$%d", i+1)
		i++
	}
	return
}
