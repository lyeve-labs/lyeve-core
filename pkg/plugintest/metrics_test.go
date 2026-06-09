package plugintest

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/require"
)

// A plugin that exports metrics gathers the engine's registry through the
// host role, and what it gets is the real one: the Go collectors and the
// pool series over the test database.
func TestHost_MetricsGatherer_ServesTheEngineRegistry(t *testing.T) {
	host := Postgres(t)
	p, ok := host.(core.MetricsGathererProvider)
	require.True(t, ok, "the test host must satisfy core.MetricsGathererProvider")

	g := p.MetricsGatherer()
	require.NotNil(t, g)
	fams, err := g.Gather()
	require.NoError(t, err)

	names := make(map[string]bool, len(fams))
	for _, f := range fams {
		names[f.GetName()] = true
	}
	for _, want := range []string{"go_goroutines", "lyeve_db_pool_max_connections", "lyeve_db_pool_open_connections"} {
		require.True(t, names[want], "registry is missing %s", want)
	}

	// A second host in the same process shares the registry. The collector
	// is registered once and follows the newest database.
	again := Postgres(t).(core.MetricsGathererProvider).MetricsGatherer()
	require.NotNil(t, again)
	_, err = again.Gather()
	require.NoError(t, err)
}
