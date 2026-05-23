//go:build integration && !mutest
// +build integration,!mutest

package testutil_test

import (
	"context"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/testutil"
)

// Test migration FS

var harnessTestMigrations = fstest.MapFS{
	"psql/001_create_items.up.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE IF NOT EXISTS sys_harness_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS _idx_harness_items_tenant ON sys_harness_items (tenant_id);
`)},
}

// TestHarness_Infrastructure validates that the harness scaffolding works:
// container startup, per-test DB isolation, Host interface, tenant contexts.
func TestHarness_Infrastructure(t *testing.T) {
	h := testutil.NewHarness(t)

	t.Run("db_is_accessible", func(t *testing.T) {
		require.NoError(t, h.DB.PingContext(context.Background()))
	})

	t.Run("host_returns_querier", func(t *testing.T) {
		ctx := context.Background()
		q := h.Host.Querier(ctx)
		require.NotNil(t, q)
	})

	t.Run("host_dialect_is_postgres", func(t *testing.T) {
		assert.Equal(t, "postgres", h.Host.Dialect())
	})

	t.Run("host_rawdb_matches_db", func(t *testing.T) {
		assert.Equal(t, h.DB, h.Host.RawDB())
	})

	t.Run("tenant_contexts_carry_distinct_ids", func(t *testing.T) {
		assert.Equal(t, "tenant_a", h.TenantIDA)
		assert.Equal(t, "tenant_b", h.TenantIDB)
		assert.NotEqual(t, h.TenantIDA, h.TenantIDB)
	})

	t.Run("tenant_contexts_extractable", func(t *testing.T) {
		assert.Equal(t, "tenant_a", core.TenantIDFromCtx(h.TenantA))
		assert.Equal(t, "tenant_b", core.TenantIDFromCtx(h.TenantB))
	})

	t.Run("host_logger_works", func(t *testing.T) {
		logger := h.Host.Logger(context.Background())
		require.NotNil(t, logger)
	})

	t.Run("host_version_is_test", func(t *testing.T) {
		assert.Equal(t, "test", h.Host.Version())
	})

	t.Run("host_schema_returns_nil", func(t *testing.T) {
		assert.Nil(t, h.Host.Schema())
	})

	t.Run("host_tracer_returns_noop", func(t *testing.T) {
		tracer := h.Host.Tracer("test")
		require.NotNil(t, tracer)
	})
}

// TestHarness_Migrations validates RunMigrations via PluginMigrate.
func TestHarness_Migrations(t *testing.T) {
	h := testutil.NewHarness(t)
	h.RunMigrations(t, harnessTestMigrations, "harness_mig_test")

	ctx := context.Background()
	var exists bool
	err := h.DB.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name = 'sys_harness_items')").Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "sys_harness_items should exist after migration")
}

// TestHarness_SeedAndCount validates SeedRow, SeedRowSQL, and CountRows.
func TestHarness_SeedAndCount(t *testing.T) {
	h := testutil.NewHarness(t)
	h.RunMigrations(t, harnessTestMigrations, "harness_seed_test")

	t.Run("seed_row_inserts", func(t *testing.T) {
		h.SeedRow(t, h.TenantIDA, "sys_harness_items",
			[]string{"id", "tenant_id", "name"},
			[]any{"11111111-1111-1111-1111-111111111111", h.TenantIDA, "item-1"})

		count := h.CountRows(t, h.TenantIDA, "sys_harness_items", "")
		assert.Equal(t, 1, count)
	})

	t.Run("seed_row_sql_inserts", func(t *testing.T) {
		h.SeedRowSQL(t, h.TenantIDA,
			"INSERT INTO sys_harness_items (id, tenant_id, name) VALUES (gen_random_uuid(), $1, $2)",
			h.TenantIDA, "item-2")

		count := h.CountRows(t, h.TenantIDA, "sys_harness_items", "tenant_id = $1", h.TenantIDA)
		assert.Equal(t, 2, count)
	})

	t.Run("count_rows_with_filter", func(t *testing.T) {
		h.SeedRow(t, h.TenantIDB, "sys_harness_items",
			[]string{"id", "tenant_id", "name"},
			[]any{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", h.TenantIDB, "b-item"})

		countA := h.CountRows(t, h.TenantIDA, "sys_harness_items", "tenant_id = $1", h.TenantIDA)
		assert.Equal(t, 2, countA)

		countB := h.CountRows(t, h.TenantIDB, "sys_harness_items", "tenant_id = $1", h.TenantIDB)
		assert.Equal(t, 1, countB)
	})
}

// TestHarness_HostQuerierExec validates Exec, QueryRow, Query through Host.
func TestHarness_HostQuerierExec(t *testing.T) {
	h := testutil.NewHarness(t)
	h.RunMigrations(t, harnessTestMigrations, "harness_exec_test")

	h.SeedRow(t, h.TenantIDA, "sys_harness_items",
		[]string{"id", "tenant_id", "name"},
		[]any{"11111111-1111-1111-1111-111111111111", h.TenantIDA, "original"})

	ctx := core.WithTenantID(context.Background(), h.TenantIDA)

	t.Run("query_row_returns_data", func(t *testing.T) {
		var name string
		row, qrErr := h.Host.Querier(ctx).QueryRow(ctx,
			"SELECT name FROM sys_harness_items WHERE id = $1",
			"11111111-1111-1111-1111-111111111111")
		require.NoError(t, qrErr, "QueryRow")
		err := row.Scan(&name)
		require.NoError(t, err)
		assert.Equal(t, "original", name)
	})

	t.Run("exec_returns_correct_rows_affected", func(t *testing.T) {
		tag, err := h.Host.Querier(ctx).Exec(ctx,
			"UPDATE sys_harness_items SET name = 'updated' WHERE id = $1",
			"11111111-1111-1111-1111-111111111111")
		require.NoError(t, err)
		assert.Equal(t, int64(1), tag.RowsAffected)
	})

	t.Run("query_returns_multiple_rows", func(t *testing.T) {
		h.SeedRow(t, h.TenantIDA, "sys_harness_items",
			[]string{"id", "tenant_id", "name"},
			[]any{"22222222-2222-2222-2222-222222222222", h.TenantIDA, "second"})

		rows, err := h.Host.Querier(ctx).Query(ctx,
			"SELECT name FROM sys_harness_items WHERE tenant_id = $1 ORDER BY name", h.TenantIDA)
		require.NoError(t, err)
		defer rows.Close()

		var names []string
		for rows.Next() {
			var n string
			require.NoError(t, rows.Scan(&n))
			names = append(names, n)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, []string{"second", "updated"}, names)
	})
}

// TestHarness_SetupTenants verifies schema-per-tenant setup.
func TestHarness_SetupTenants(t *testing.T) {
	h := testutil.NewHarness(t)
	h.SetupTenants(t, "tenant_a", "tenant_b")

	ctx := context.Background()
	for _, tid := range []string{"tenant_a", "tenant_b"} {
		var exists bool
		err := h.DB.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)",
			"tenant_"+tid).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, fmt.Sprintf("schema tenant_%s should exist", tid))
	}
}

// TestHarness_MultipleHarnessesAreIsolated verifies that two Harness
// instances get independent databases.
func TestHarness_MultipleHarnessesAreIsolated(t *testing.T) {
	h1 := testutil.NewHarness(t)
	h2 := testutil.NewHarness(t)

	h1.RunMigrations(t, harnessTestMigrations, "harness_iso_1")
	h2.RunMigrations(t, harnessTestMigrations, "harness_iso_2")

	h1.SeedRow(t, h1.TenantIDA, "sys_harness_items",
		[]string{"id", "tenant_id", "name"},
		[]any{"11111111-1111-1111-1111-111111111111", h1.TenantIDA, "h1-item"})

	count := h2.CountRows(t, h2.TenantIDA, "sys_harness_items", "")
	assert.Equal(t, 0, count, "h2 database should be empty")
}

// TestHarness_TenantIDPropagation demonstrates that TenantIDFromCtx extracts
// the tenant ID from the harness contexts. Plugin stores use this to build
// WHERE clauses like: WHERE ... AND tenant_id = $N.
func TestHarness_TenantIDPropagation(t *testing.T) {
	h := testutil.NewHarness(t)

	// This is how plugin stores extract the tenant for their queries:
	tenantID := core.TenantIDFromCtx(h.TenantA)
	assert.Equal(t, "tenant_a", tenantID)

	// And this is the pattern for cross-tenant IDOR testing:
	// 1. Create data with tenantA's ctx (store.Create sets tenant_id from ctx)
	// 2. Read with tenantB's ctx (store.GetByID should filter tenant_id)
	// 3. If the store doesn't filter -> IDOR (data leaks across tenants)
	// 4. A store that filters returns ErrNotFound for the wrong tenant

	ctx := context.Background()
	assert.Equal(t, "", core.TenantIDFromCtx(ctx), "bare context has no tenant")
}
