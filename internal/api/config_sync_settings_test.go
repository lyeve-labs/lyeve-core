package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The engine settings a configuration bundle carries: what an export writes,
// what a plan says about them, and an apply that keeps every other key the
// admin layer stored, sealed credentials included.

func settingsPools(t *testing.T) map[string]func(*testing.T) db.DB {
	t.Helper()
	pools := map[string]func(*testing.T) db.DB{}
	for name, open := range map[string]func(*testing.T) db.DB{
		"postgres": testdb.Postgres, "mysql": testdb.MySQL, "mssql": testdb.MSSQL,
	} {
		if testdb.ShouldTest(name) {
			pools[name] = open
		}
	}
	return pools
}

func TestSettingsSection_RoundTripKeepsWhatTheBundleDoesNotCarry(t *testing.T) {
	withResolver(t, nil)
	for name, open := range settingsPools(t) {
		t.Run(name, func(t *testing.T) {
			ctx := core.WithTenantID(context.Background(), "default")
			sealer := db.NewConfigSealer("test-master-key-for-config-sync-settings")

			srcPool := open(t)
			src := db.NewPluginConfigStore(srcPool).WithSealer(sealer)
			require.NoError(t, src.Set(ctx, config.CoreSettingsOwner, map[string]any{
				"RATE_LIMIT_RPS":  "250",
				"JWT_EXPIRY_SECS": "600",
				"CORS_ORIGINS":    "https://staging.example.com",
			}, nil))

			dstPool := open(t)
			dst := db.NewPluginConfigStore(dstPool).WithSealer(sealer)
			require.NoError(t, dst.Set(ctx, config.CoreSettingsOwner, map[string]any{
				"RATE_LIMIT_RPS":   "100",
				"CORS_ORIGINS":     "https://www.example.com",
				"EXAMPLE_PASSWORD": "kept-secret",
			}, nil))

			exported, err := newSettingsSection(src, nil, srcPool.Engine()).ExportConfig(ctx, core.ConfigOptions{})
			require.NoError(t, err)
			var carried map[string]any
			require.NoError(t, json.Unmarshal(exported, &carried))
			assert.Equal(t, map[string]any{"RATE_LIMIT_RPS": "250", "JWT_EXPIRY_SECS": "600"}, carried,
				"the export carried a setting that names where the instance lives")

			actor := uuid.New()
			applyCtx := context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: actor.String()})

			section := newSettingsSection(dst, nil, dstPool.Engine())
			plan, err := section.PlanConfig(ctx, exported, core.ConfigOptions{})
			require.NoError(t, err)
			require.Empty(t, plan.Problems)
			assert.ElementsMatch(t, []core.ConfigChange{
				{Key: "JWT_EXPIRY_SECS", Action: core.ConfigCreate},
				{Key: "RATE_LIMIT_RPS", Action: core.ConfigUpdate, Fields: []string{"value"}},
			}, plan.Changes)

			q := testhost.New(dstPool).Querier(ctx)
			tx, err := q.Begin(ctx)
			require.NoError(t, err)
			applied, err := section.ApplyConfig(applyCtx, tx, exported, core.ConfigOptions{})
			require.NoError(t, err)
			require.NoError(t, tx.Commit(ctx))
			assert.Len(t, applied.Changes, 2)
			require.NotNil(t, applied.AfterCommit)

			row, err := dstPool.QueryRow(ctx,
				`SELECT COUNT(*) FROM sys_plugin_config WHERE tenant_id = $1 AND plugin_name = $2 AND updated_by = $3`,
				"default", config.CoreSettingsOwner, &actor)
			require.NoError(t, err)
			var attributed int
			require.NoError(t, row.Scan(&attributed))
			assert.Equal(t, 1, attributed, "the applied settings row does not name the user who applied the bundle")

			got, err := dst.GetResolved(ctx, config.CoreSettingsOwner)
			require.NoError(t, err)
			assert.Equal(t, "250", got["RATE_LIMIT_RPS"])
			assert.Equal(t, "600", got["JWT_EXPIRY_SECS"])
			assert.Equal(t, "https://www.example.com", got["CORS_ORIGINS"], "an origin the bundle never carried was overwritten")
			assert.Equal(t, "kept-secret", got["EXAMPLE_PASSWORD"], "a sealed credential did not survive the rewrite of its row")

			again, err := section.PlanConfig(ctx, exported, core.ConfigOptions{})
			require.NoError(t, err)
			for _, c := range again.Changes {
				assert.Equal(t, core.ConfigUnchanged, c.Action, "%s still differs after the apply", c.Key)
			}
		})
	}
}

func TestSettingsSection_PlanRefusesWhatThisInstanceCannotTake(t *testing.T) {
	t.Setenv("RATE_LIMIT_RPS", "777")
	withResolver(t, nil)
	pool := testdb.Postgres(t)
	ctx := core.WithTenantID(context.Background(), "default")
	section := newSettingsSection(db.NewPluginConfigStore(pool), nil, pool.Engine())

	plan, err := section.PlanConfig(ctx, json.RawMessage(`{"RATE_LIMIT_RPS":"5","DATABASE_URL":"postgres://elsewhere"}`), core.ConfigOptions{})
	require.NoError(t, err)
	keys := map[string]bool{}
	for _, p := range plan.Problems {
		keys[p.Key] = true
	}
	assert.True(t, keys["RATE_LIMIT_RPS"], "a setting the environment pins was planned as a change")
	assert.True(t, keys["DATABASE_URL"], "a setting off the portable list was accepted")
}

func TestSettingsSection_PruneRemovesOnlyPortableKeys(t *testing.T) {
	withResolver(t, nil)
	pool := testdb.Postgres(t)
	ctx := core.WithTenantID(context.Background(), "default")
	store := db.NewPluginConfigStore(pool)
	require.NoError(t, store.Set(ctx, config.CoreSettingsOwner, map[string]any{
		"MAX_BODY_BYTES": "2048", "CORS_ORIGINS": "https://www.example.com",
	}, nil))

	plan, err := newSettingsSection(store, nil, pool.Engine()).PlanConfig(ctx, json.RawMessage(`{}`), core.ConfigOptions{Prune: true})
	require.NoError(t, err)
	assert.Equal(t, []core.ConfigChange{{Key: "MAX_BODY_BYTES", Action: core.ConfigDelete}}, plan.Changes)
}
