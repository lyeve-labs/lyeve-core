package plugintest_test

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

type purgingPlugin struct{ table string }

func (p purgingPlugin) Name() string                           { return "purging" }
func (p purgingPlugin) Start(context.Context, core.Host) error { return nil }
func (p purgingPlugin) Stop(context.Context) error             { return nil }

func (p purgingPlugin) TenantPurges() []core.TenantPurge {
	return []core.TenantPurge{{
		Tables: []string{p.table},
		Handler: func(ctx context.Context, q core.Querier, slug string) error {
			_, err := q.Exec(ctx, `DELETE FROM `+p.table+` WHERE tenant_id = $1`, slug)
			return err
		},
	}}
}

func TestRunTenantPurges_ClearsTheTenantsRows(t *testing.T) {
	host := plugintest.Postgres(t)
	ctx := context.Background()
	q := host.Querier(ctx)
	table := "purging_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
	_, err := q.Exec(ctx, `CREATE TABLE `+table+` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
	require.NoError(t, err)
	_, err = q.Exec(ctx, `INSERT INTO `+table+` (id, tenant_id) VALUES ('1', 'acme'), ('2', 'other')`)
	require.NoError(t, err)

	covered := plugintest.RunTenantPurges(t, host, purgingPlugin{table: table}, "acme")
	assert.True(t, covered[table])
	assert.Zero(t, core.TenantPurgeHandlerCount(), "the registry is left empty")

	row, err := q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = 'acme'`)
	require.NoError(t, err)
	var n int
	require.NoError(t, row.Scan(&n))
	assert.Zero(t, n)
	row, err = q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE tenant_id = 'other'`)
	require.NoError(t, err)
	require.NoError(t, row.Scan(&n))
	assert.Equal(t, 1, n)
}

type outcomePlugin struct {
	table     string
	committed *[]bool
}

func (p outcomePlugin) Name() string                           { return "outcome" }
func (p outcomePlugin) Start(context.Context, core.Host) error { return nil }
func (p outcomePlugin) Stop(context.Context) error             { return nil }

func (p outcomePlugin) TenantPurges() []core.TenantPurge {
	return []core.TenantPurge{{
		Tables: []string{p.table},
		Handler: func(ctx context.Context, _ core.Querier, _ string) error {
			core.OnTenantPurgeOutcome(ctx, func(_ context.Context, committed bool) {
				*p.committed = append(*p.committed, committed)
			})
			return nil
		},
	}}
}

func TestRunTenantPurges_EndsThePurgeWithACommit(t *testing.T) {
	host := plugintest.Postgres(t)
	ctx := context.Background()
	table := "outcome_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
	_, err := host.Querier(ctx).Exec(ctx, `CREATE TABLE `+table+` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
	require.NoError(t, err)
	var committed []bool

	plugintest.RunTenantPurges(t, host, outcomePlugin{table: table, committed: &committed}, "acme")

	assert.Equal(t, []bool{true}, committed)
}
