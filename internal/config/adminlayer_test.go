package config

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type fakeAdminStore struct {
	rows []db.PluginConfigRow
	err  error
}

func (f fakeAdminStore) ListAll(context.Context) ([]db.PluginConfigRow, error) {
	return f.rows, f.err
}

func TestBuildAdminLayer_FlattensToResolverKeys(t *testing.T) {
	layer := BuildAdminLayer([]db.PluginConfigRow{
		{PluginName: "example", Config: map[string]any{"example_host": "mail.example.com"}},
	})

	assert.Equal(t, map[string]string{"EXAMPLE_HOST": "mail.example.com"}, layer.Values)
	assert.Empty(t, layer.Conflicts)
}

func TestBuildAdminLayer_SkipsTenantScopedRows(t *testing.T) {
	// Plugins start once per process, before any request has identified a
	// tenant, so a per-tenant row has nothing to configure.
	layer := BuildAdminLayer([]db.PluginConfigRow{
		{TenantID: "", PluginName: "example", Config: map[string]any{"example_host": "default"}},
		{TenantID: "acme", PluginName: "example", Config: map[string]any{"example_host": "acme-only"}},
	})

	assert.Equal(t, "default", layer.Values["EXAMPLE_HOST"])
}

// A save through the admin API carries the request's tenant, and on a
// single-tenant install that is the "default" slug rather than the empty
// string. A row saved under it must reach the resolver, or the endpoint goes
// on reporting the file value.
func TestBuildAdminLayer_AppliesRowsSavedUnderTheDefaultTenant(t *testing.T) {
	layer := BuildAdminLayer([]db.PluginConfigRow{
		{TenantID: core.DefaultTenantSlug, PluginName: "core", Config: map[string]any{"storage_driver": "s3"}},
	})

	assert.Equal(t, "s3", layer.Values["STORAGE_DRIVER"])
}

func TestBuildAdminLayer_RendersValuesAsTheEnvironmentWould(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "a string passes through", value: "text", want: "text"},
		{name: "a true boolean", value: true, want: "true"},
		{name: "a false boolean", value: false, want: "false"},
		{name: "a whole number keeps no decimal point", value: 30.0, want: "30"},
		{name: "a fractional number", value: 1.5, want: "1.5"},
		{name: "null is empty", value: nil, want: ""},
		{name: "a list joins on commas", value: []any{"a", "b"}, want: "a,b"},
		{name: "a list of numbers", value: []any{1.0, 2.0}, want: "1,2"},
		{name: "an object falls back to JSON", value: map[string]any{"k": "v"}, want: `{"k":"v"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			layer := BuildAdminLayer([]db.PluginConfigRow{
				{PluginName: "p", Config: map[string]any{"key": tc.value}},
			})
			assert.Equal(t, tc.want, layer.Values["KEY"])
		})
	}
}

func TestBuildAdminLayer_ReportsKeysTwoPluginsClaim(t *testing.T) {
	// Keys are global because the names they resolve under are, so two plugins
	// declaring one already shared a setting. Say so rather than resolving it
	// silently.
	layer := BuildAdminLayer([]db.PluginConfigRow{
		{PluginName: "analytics", Config: map[string]any{"cache_ttl": "5m"}},
		{PluginName: "search", Config: map[string]any{"cache_ttl": "10m"}},
	})

	require.Len(t, layer.Conflicts, 1)
	assert.Equal(t, "CACHE_TTL: analytics, search", layer.Conflicts[0])
}

func TestBuildAdminLayer_ReportsUndecryptableCredentials(t *testing.T) {
	layer := BuildAdminLayer([]db.PluginConfigRow{
		{PluginName: "stripe", Config: map[string]any{}, UndecryptableKeys: []string{"api_key"}},
	})

	assert.Equal(t, []string{"stripe.api_key"}, layer.Undecryptable)
}

func TestRefreshAdminLayer_InstallsOnTheActiveResolver(t *testing.T) {
	prev := SetActiveResolver(NewResolver(nil))
	t.Cleanup(func() { SetActiveResolver(prev) })

	layer, err := RefreshAdminLayer(context.Background(), fakeAdminStore{
		rows: []db.PluginConfigRow{
			{PluginName: "stripe", Config: map[string]any{"stripe_api_key": "sk_live_x"}},
		},
	})
	require.NoError(t, err)
	assert.Len(t, layer.Values, 1)

	assert.Equal(t, "sk_live_x", ActiveResolver().Get("stripe_api_key"),
		"a saved value reaches the running engine without a restart")
}

func TestRefreshAdminLayer_ReplacesRatherThanMerges(t *testing.T) {
	prev := SetActiveResolver(NewResolver(nil))
	t.Cleanup(func() { SetActiveResolver(prev) })

	_, err := RefreshAdminLayer(context.Background(), fakeAdminStore{
		rows: []db.PluginConfigRow{{PluginName: "p", Config: map[string]any{"gone_after": "x"}}},
	})
	require.NoError(t, err)
	require.Equal(t, "x", ActiveResolver().Get("gone_after"))

	_, err = RefreshAdminLayer(context.Background(), fakeAdminStore{rows: nil})
	require.NoError(t, err)
	assert.Equal(t, "", ActiveResolver().Get("gone_after"),
		"a deleted setting stops applying rather than lingering")
}

func TestRefreshAdminLayer_StoreFailureLeavesTheLayerAlone(t *testing.T) {
	prev := SetActiveResolver(NewResolver(nil))
	t.Cleanup(func() { SetActiveResolver(prev) })
	ActiveResolver().SetAdminLayer(map[string]string{"keep_me": "yes"})

	_, err := RefreshAdminLayer(context.Background(), fakeAdminStore{err: errors.New("db down")})
	require.Error(t, err)
	assert.Equal(t, "yes", ActiveResolver().Get("keep_me"),
		"a failed reload must not blank the configuration a running engine is using")
}

func TestRefreshAdminLayer_NilStoreIsNotAnError(t *testing.T) {
	layer, err := RefreshAdminLayer(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, layer.Values)
}
