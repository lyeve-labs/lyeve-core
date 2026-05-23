package plugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// regionHolder is a plugin of any name that answers a tenant's region from a
// fixed table. It imports nothing but pkg/core, which is all a third party
// needs to hold the role.
type regionHolder struct {
	*fakePlugin
	regions map[string]string
}

func (p *regionHolder) TenantRegionResolver() core.TenantRegionResolver { return p }

func (p *regionHolder) TenantRegion(_ context.Context, tenantID string) (string, bool, error) {
	r, ok := p.regions[tenantID]
	return r, ok, nil
}

// regionHost is a host that hands plugins the activator's region role, the
// way the runtime wires the engine host.
type regionHost struct {
	testHost
	src core.TenantRegionResolverProvider
}

func (h regionHost) TenantRegionResolver() core.TenantRegionResolver {
	return h.src.TenantRegionResolver()
}

func TestTenantRegionResolver_NoImplementerIsNil(t *testing.T) {
	a := startRolePlugins(t, newFakePlugin("bystander"))

	assert.Nil(t, a.TenantRegionResolver())
}

func TestTenantRegionResolver_ComesFromThePluginThatImplementsIt(t *testing.T) {
	a := startRolePlugins(t, newFakePlugin("bystander"),
		&regionHolder{fakePlugin: newFakePlugin("any-name"), regions: map[string]string{"acme": "eu-west-1"}})

	r := a.TenantRegionResolver()
	require.NotNil(t, r)
	region, assigned, err := r.TenantRegion(context.Background(), "acme")
	require.NoError(t, err)
	assert.True(t, assigned)
	assert.Equal(t, "eu-west-1", region)

	_, assigned, err = r.TenantRegion(context.Background(), "globex")
	require.NoError(t, err)
	assert.False(t, assigned, "a tenant the holder has no region for")
}

func TestTenantRegionResolver_TwoImplementersRefuseTheBoot(t *testing.T) {
	a := startRolePlugins(t,
		&regionHolder{fakePlugin: newFakePlugin("regions-b")},
		&regionHolder{fakePlugin: newFakePlugin("regions-a")},
	)

	err := a.CheckRoles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "core.TenantRegionResolverProvider")
	assert.Nil(t, a.TenantRegionResolver(), "a role two plugins claim is wired from neither")
}

// Another plugin reads the role through its scoped host. The host asks the
// activator on every call, so the answer follows the holder: present while it
// runs and gone once it stops, with nothing kept from an earlier call.
func TestTenantRegionResolver_ReachesOtherPluginsThroughTheHostWhileTheHolderRuns(t *testing.T) {
	a := startRolePlugins(t,
		&regionHolder{fakePlugin: newFakePlugin("any-name"), regions: map[string]string{"acme": "eu-west-1"}})
	scoped := core.NewScopedHost(regionHost{src: a}, "reader", 0)

	var host any = scoped
	rp, ok := host.(core.TenantRegionResolverProvider)
	require.True(t, ok, "a plugin's host offers the region role")
	r := rp.TenantRegionResolver()
	require.NotNil(t, r)
	region, assigned, err := r.TenantRegion(context.Background(), "acme")
	require.NoError(t, err)
	assert.True(t, assigned)
	assert.Equal(t, "eu-west-1", region)

	require.NoError(t, a.Stop(context.Background()))
	assert.Nil(t, rp.TenantRegionResolver(), "the role outlived the plugin that held it")
}
