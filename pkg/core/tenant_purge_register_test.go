package core_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// registeredPurge registers tp alone and returns the handler the delete
// transaction would call.
func registeredPurge(t *testing.T, dialect string, tp core.TenantPurge) core.TenantPurgeHandler {
	t.Helper()
	core.ResetTenantPurgeHandlers()
	core.ResetCoveredTables()
	t.Cleanup(func() {
		core.ResetTenantPurgeHandlers()
		core.ResetCoveredTables()
	})
	core.RegisterTenantPurge(dialect, tp)
	handlers := core.TenantPurgeHandlers()
	require.Len(t, handlers, 1)
	return handlers[0]
}

// A plugin that never ran here has no tables, and its purge still runs on
// every tenant delete. It has to leave the delete transaction usable, which
// on Postgres a failed statement would not.
func TestRegisterTenantPurge_AbsentTablesSkipTheHandler(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
		called := false
		h := registeredPurge(t, host.Dialect(), core.TenantPurge{
			Tables: []string{"never_created_a_" + suffix, "never_created_b_" + suffix},
			Handler: func(context.Context, core.Querier, string) error {
				called = true
				return nil
			},
		})

		tx, err := host.Querier(ctx).Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		require.NoError(t, h(ctx, tx, "acme"))
		assert.False(t, called, "a purge whose tables are all absent runs no handler")

		row, err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM sys_users`)
		require.NoError(t, err)
		var n int
		require.NoError(t, row.Scan(&n), "the transaction is still usable")

		covered := core.CoveredTableSet()
		assert.True(t, covered["never_created_a_"+suffix])
		assert.True(t, covered["never_created_b_"+suffix])
	})
}

func TestRegisterTenantPurge_PresentTablesRunTheHandler(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		table := "purge_present_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
		_, err := q.Exec(ctx, `CREATE TABLE `+table+` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+table) })
		_, err = q.Exec(ctx, `INSERT INTO `+table+` (id, tenant_id) VALUES ('1', 'acme'), ('2', 'other')`)
		require.NoError(t, err)

		h := registeredPurge(t, host.Dialect(), core.TenantPurge{
			// The catalog may fold the name's case, and the check must still
			// find it.
			Tables: []string{strings.ToUpper(table)},
			Handler: func(ctx context.Context, q core.Querier, slug string) error {
				_, err := q.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1`, slug)
				return err
			},
		})

		tx, err := q.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, h(ctx, tx, "acme"))
		require.NoError(t, tx.Commit(ctx))

		var acme, other int
		row, err := q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = 'acme'`)
		require.NoError(t, err)
		require.NoError(t, row.Scan(&acme))
		row, err = q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = 'other'`)
		require.NoError(t, err)
		require.NoError(t, row.Scan(&other))
		assert.Equal(t, 0, acme, "the deleted tenant's rows")
		assert.Equal(t, 1, other, "another tenant's rows")
	})
}

// A table a later migration added is absent where the plugin last ran at an
// older version. Running the handler would fail on it and roll the delete
// back with a driver error, so the purge names what is missing instead.
func TestRegisterTenantPurge_PartlyPresentTablesRefuse(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		suffix := strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
		present := "purge_partial_" + suffix
		absent := "purge_missing_" + suffix
		_, err := q.Exec(ctx, `CREATE TABLE `+present+` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+present) })

		called := false
		h := registeredPurge(t, host.Dialect(), core.TenantPurge{
			Tables: []string{present, absent},
			Handler: func(context.Context, core.Querier, string) error {
				called = true
				return nil
			},
		})
		err = h(ctx, q, "acme")
		require.Error(t, err)
		assert.Contains(t, err.Error(), absent)
		assert.False(t, called)
	})
}

func TestRegisterTenantPurge_IgnoresAnEmptyPurge(t *testing.T) {
	core.ResetTenantPurgeHandlers()
	core.ResetCoveredTables()
	t.Cleanup(core.ResetTenantPurgeHandlers)
	t.Cleanup(core.ResetCoveredTables)

	core.RegisterTenantPurge("postgres", core.TenantPurge{Tables: []string{"t"}})
	core.RegisterTenantPurge("postgres", core.TenantPurge{Handler: func(context.Context, core.Querier, string) error { return nil }})
	assert.Equal(t, 0, core.TenantPurgeHandlerCount())
	assert.Empty(t, core.CoveredTableSet())
}

// createTable makes a table for one test and drops it afterwards.
func createTable(t *testing.T, q core.Querier, prefix, columns string) string {
	t.Helper()
	ctx := context.Background()
	name := prefix + strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
	_, err := q.Exec(ctx, `CREATE TABLE `+name+` (`+columns+`)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+name) })
	return name
}

func countRows(t *testing.T, q core.Querier, table string) int {
	t.Helper()
	row, err := q.QueryRow(context.Background(), `SELECT COUNT(*) FROM `+table)
	require.NoError(t, err)
	var n int
	require.NoError(t, row.Scan(&n))
	return n
}

// Where the plugin last ran before the migration that added tenant_id, its
// table holds rows no tenant owns. The delete goes ahead without touching
// them, and the transaction stays usable for the rest of the purge.
func TestRegisterTenantPurge_AbsentColumnSkipsTheHandler(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		table := createTable(t, q, "purge_unkeyed_", `id VARCHAR(36) PRIMARY KEY, label VARCHAR(64)`)
		_, err := q.Exec(ctx, `INSERT INTO `+table+` (id, label) VALUES ('1', 'before tenancy')`)
		require.NoError(t, err)

		called := false
		h := registeredPurge(t, host.Dialect(), core.TenantPurge{
			Tables:  []string{table},
			Columns: []string{table + ".tenant_id"},
			Handler: func(ctx context.Context, q core.Querier, slug string) error {
				called = true
				_, err := q.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1`, slug)
				return err
			},
		})

		tx, err := q.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, h(ctx, tx, "acme"))
		assert.False(t, called, "a set whose tenant column is absent runs no handler")
		assert.Equal(t, 1, countRows(t, tx, table), "the transaction is still usable")
		require.NoError(t, tx.Commit(ctx))
		assert.Equal(t, 1, countRows(t, q, table), "rows no tenant owns stay")
		assert.True(t, core.CoveredTableSet()[table])
	})
}

// A migration that assigned existing rows to a tenant rather than to none
// makes those rows that tenant's, so its delete clears them through Unkeyed.
func TestRegisterTenantPurge_AbsentColumnRunsUnkeyed(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		table := createTable(t, q, "purge_backfill_", `id VARCHAR(36) PRIMARY KEY`)
		_, err := q.Exec(ctx, `INSERT INTO `+table+` (id) VALUES ('1')`)
		require.NoError(t, err)

		var unkeyedFor []string
		h := registeredPurge(t, host.Dialect(), core.TenantPurge{
			Tables:  []string{table},
			Columns: []string{table + ".tenant_id"},
			Handler: func(context.Context, core.Querier, string) error {
				t.Error("the keyed handler ran without its column")
				return nil
			},
			Unkeyed: func(ctx context.Context, q core.Querier, slug string) error {
				unkeyedFor = append(unkeyedFor, slug)
				if slug != core.DefaultTenantSlug {
					return nil
				}
				_, err := q.Exec(ctx, `DELETE FROM `+table)
				return err
			},
		})

		require.NoError(t, h(ctx, q, "acme"))
		assert.Equal(t, 1, countRows(t, q, table))
		require.NoError(t, h(ctx, q, core.DefaultTenantSlug))
		assert.Equal(t, 0, countRows(t, q, table))
		assert.Equal(t, []string{"acme", core.DefaultTenantSlug}, unkeyedFor)
	})
}

// Once the migration has run, the column exists and the handler runs.
func TestRegisterTenantPurge_PresentColumnRunsTheHandler(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		table := createTable(t, q, "purge_keyed_", `id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL`)
		_, err := q.Exec(ctx, `INSERT INTO `+table+` (id, tenant_id) VALUES ('1', 'acme'), ('2', 'other')`)
		require.NoError(t, err)

		h := registeredPurge(t, host.Dialect(), core.TenantPurge{
			Tables:  []string{table},
			Columns: []string{strings.ToUpper(table) + ".TENANT_ID"},
			Handler: func(ctx context.Context, q core.Querier, slug string) error {
				_, err := q.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1`, slug)
				return err
			},
		})
		require.NoError(t, h(ctx, q, "acme"))
		assert.Equal(t, 1, countRows(t, q, table))
	})
}

// A set whose column exists on one table and not another cannot be purged
// whole, so the delete is refused and names the missing column.
func TestRegisterTenantPurge_PartlyPresentColumnsRefuse(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		keyed := createTable(t, q, "purge_halfa_", `id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL`)
		unkeyed := createTable(t, q, "purge_halfb_", `id VARCHAR(36) PRIMARY KEY`)

		called := false
		h := registeredPurge(t, host.Dialect(), core.TenantPurge{
			Tables:  []string{keyed, unkeyed},
			Columns: []string{keyed + ".tenant_id", unkeyed + ".tenant_id"},
			Handler: func(context.Context, core.Querier, string) error {
				called = true
				return nil
			},
		})
		err := h(ctx, q, "acme")
		require.Error(t, err)
		assert.Contains(t, err.Error(), unkeyed+".tenant_id")
		assert.NotContains(t, err.Error(), keyed+".tenant_id")
		assert.False(t, called)
	})
}
