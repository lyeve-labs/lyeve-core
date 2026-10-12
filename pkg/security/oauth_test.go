package security

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The discovery cache serves every login against one issuer. The fetch itself
// is not reachable from a test, because the client refuses loopback
// addresses, so the entry is placed in the cache the way a fetch leaves it.
func TestDiscoverOIDC_EachCallerGetsItsOwnCopy(t *testing.T) {
	const issuer = "https://issuer.example.test"
	oidcDiscoveryMu.Lock()
	oidcDiscoveryCache[issuer] = &oidcDiscoveryEntry{
		cfg:  &OIDCConfig{Issuer: issuer, TokenEndpoint: issuer + "/token"},
		time: time.Now(),
	}
	oidcDiscoveryMu.Unlock()
	t.Cleanup(func() {
		oidcDiscoveryMu.Lock()
		delete(oidcDiscoveryCache, issuer)
		oidcDiscoveryMu.Unlock()
	})

	first, err := DiscoverOIDC(context.Background(), issuer)
	require.NoError(t, err)
	first.TokenEndpoint = "https://elsewhere.example.test/token"

	second, err := DiscoverOIDC(context.Background(), issuer)
	require.NoError(t, err)
	assert.Equal(t, issuer+"/token", second.TokenEndpoint, "one caller's edit reached the next login through the cache")
}
