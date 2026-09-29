package licensing

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openManagerFor(t *testing.T, names ...string) Manager {
	t.Helper()
	m, err := Open().NewManager(context.Background(), Env{Names: names})
	require.NoError(t, err)
	return m
}

func TestOpen_GrantsExactlyTheNamesTheBuildCompiled(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  []string
	}{
		{name: "no plugin compiled", names: nil, want: []string{}},
		{name: "sorted and without repeats", names: []string{"search", "audit", "search", ""}, want: []string{"audit", "search"}},
		{name: "a declared name beside a plugin", names: []string{"reports", "reports-export"}, want: []string{"reports", "reports-export"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := openManagerFor(t, tc.names...).Snapshot()
			assert.Equal(t, tc.want, snap.Features)
			assert.Equal(t, "free", snap.Plan)
			assert.Equal(t, "free", snap.State)
			assert.Zero(t, snap.TenantQuota, "no ceiling on tenants")
			assert.NotNil(t, snap.Caps)
			assert.Empty(t, snap.Caps, "no ceiling of any kind")
			assert.Empty(t, snap.Source)
			assert.Nil(t, snap.ExpiresAt)
			assert.Empty(t, snap.Problem)
		})
	}
}

// The endpoint serves the snapshot as it is, so an empty build still answers
// arrays and an object rather than null.
func TestOpen_SnapshotEncodesNoNull(t *testing.T) {
	b, err := json.Marshal(openManagerFor(t).Snapshot())
	require.NoError(t, err)
	assert.JSONEq(t, `{"plan":"free","state":"free","features":[],"tenant_quota":0,"caps":{}}`, string(b))
}

func TestOpen_SnapshotIsACopy(t *testing.T) {
	m := openManagerFor(t, "audit", "search")
	first := m.Snapshot()
	first.Features[0] = "changed"
	assert.Equal(t, []string{"audit", "search"}, m.Snapshot().Features)
}

func TestOpen_StartsEveryPluginUngated(t *testing.T) {
	m := openManagerFor(t, "audit")
	for _, name := range []string{"audit", "a-plugin-it-was-not-told-about", ""} {
		assert.Equal(t, PluginGrant{Start: true, Ungated: true}, m.Plugin(name), name)
	}
}

func TestOpen_WithholdsNothingFromAnyTenant(t *testing.T) {
	m := openManagerFor(t, "audit")
	for _, tenant := range []string{"", "default", "acme"} {
		assert.False(t, m.Withholds(tenant, "audit"), tenant)
		got := m.WithheldFrom(tenant)
		assert.NotNil(t, got, tenant)
		assert.Empty(t, got, tenant)
	}
}

func TestOpen_ServesNoRoutesAndWatchesNoPlugins(t *testing.T) {
	m := openManagerFor(t, "audit")
	_, routes := m.(RouteProvider)
	assert.False(t, routes, "a build with no licensing implementation serves no license routes")
	_, observes := m.(StartObserver)
	assert.False(t, observes)

	changed := false
	m.OnChange(func(Change) { changed = true })
	m.Start(context.Background())
	assert.False(t, changed, "a license that cannot change never reports a change")
}

// Open grants what the build compiled and nothing more, whatever an operator
// sets. The credential, the cache directory, the instance id and the license
// server are everything the environment hands a verifier, and none of them
// widens what Open grants or narrows what it starts.
func TestOpen_NoCredentialChangesWhatItGrants(t *testing.T) {
	names := []string{"audit", "search"}
	want := openManagerFor(t, names...)
	credentials := map[string]string{
		"nothing":       "",
		"a token":       "eyJ.not-a-real.token",
		"an opaque key": "an-opaque-license-key-00112233445566778899aabbccddeeff",
		"garbage":       "not-a-token",
	}
	for name, credential := range credentials {
		t.Run(name, func(t *testing.T) {
			m, err := Open().NewManager(context.Background(), Env{
				Credential: credential,
				CacheDir:   t.TempDir(),
				InstanceID: "inst-1",
				ServerURL:  "https://license.example",
				Names:      names,
			})
			require.NoError(t, err)
			assert.Equal(t, want.Snapshot(), m.Snapshot())
			for _, n := range append(append([]string{}, names...), "a-plugin-it-was-not-told-about") {
				assert.Equal(t, want.Plugin(n), m.Plugin(n), n)
				assert.False(t, m.Withholds("acme", n), n)
			}
		})
	}
}
