package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// CACHE_DRIVER is the cache plugin's setting, and redis is its recommended
// value for several replicas. The engine's content caches stay in process
// memory whatever it says, so content reads are cached with either value.
func TestAPIRouter_ContentIsCachedWhateverTheCacheDriver(t *testing.T) {
	for _, driver := range []string{"memory", "redis"} {
		t.Run(driver, func(t *testing.T) {
			t.Setenv("CACHE_DRIVER", driver)
			cfg := testConfig()
			var store *db.ContentStore
			_, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil, WithLifetime(testLifetime(t)),
				WithContentStoreSink(func(cs *db.ContentStore) { store = cs }))
			require.NoError(t, err)
			require.NotNil(t, store)
			require.True(t, store.Caching(), "content reads are not cached with CACHE_DRIVER=%s", driver)
		})
	}
}
