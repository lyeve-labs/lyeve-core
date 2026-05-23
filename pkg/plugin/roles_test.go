package plugin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// testRole is a role no engine code reads, so these tests own it.
type testRole interface{ HolderName() string }

type roleHolder struct{ *fakePlugin }

func (p *roleHolder) HolderName() string { return p.name }

// captchaHolder holds a single role the engine really wires, under whatever
// name it is registered with. Its middleware marks the requests it sees.
type captchaHolder struct {
	*fakePlugin
	seen int
}

func (p *captchaHolder) CaptchaMiddleware(context.Context) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.seen++
			next.ServeHTTP(w, r)
		})
	}
}

// preAuthHolder contributes to a role any number of plugins may implement.
type preAuthHolder struct{ *fakePlugin }

func (p *preAuthHolder) PreAuthMiddleware() []func(http.Handler) http.Handler { return nil }

// startRolePlugins registers plugins, grants them and starts them.
func startRolePlugins(t *testing.T, plugins ...core.Plugin) *Activator {
	t.Helper()
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		RegisterPlugin(p.Name(), func() core.Plugin { return p })
		names = append(names, p.Name())
	}
	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(grants(names...), "")
	require.NoError(t, a.Start(context.Background()))
	return a
}

func TestOnly_NoImplementerIsNoOwnerAndNoError(t *testing.T) {
	a := startRolePlugins(t, newFakePlugin("alpha"))

	_, ok, err := Only[testRole](a)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestOnly_ThePluginThatImplementsTheRoleHoldsIt(t *testing.T) {
	a := startRolePlugins(t, newFakePlugin("alpha"), &roleHolder{newFakePlugin("any-name")})

	holder, ok, err := Only[testRole](a)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "any-name", holder.HolderName(), "the role is found by its interface, whatever the plugin is called")
}

func TestOnly_TwoImplementersAreAnErrorNamingBoth(t *testing.T) {
	a := startRolePlugins(t, &roleHolder{newFakePlugin("beta")}, &roleHolder{newFakePlugin("alpha")})

	_, ok, err := Only[testRole](a)
	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "plugin.testRole")
	assert.Contains(t, err.Error(), "(alpha, beta)", "both claimants are named, in name order")
}

func TestOnly_APluginThatFailedToStartHoldsNothing(t *testing.T) {
	failed := &roleHolder{newFakePlugin("broken")}
	failed.startErr = errors.New("simulated start failure")
	a := startRolePlugins(t, failed, &roleHolder{newFakePlugin("working")})

	holder, ok, err := Only[testRole](a)
	require.NoError(t, err, "a plugin that is not running claims no role")
	require.True(t, ok)
	assert.Equal(t, "working", holder.HolderName())
}

func TestProviders_ReturnsEveryRunningImplementerInNameOrder(t *testing.T) {
	a := startRolePlugins(t,
		&roleHolder{newFakePlugin("gamma")},
		newFakePlugin("beta"),
		&roleHolder{newFakePlugin("alpha")},
	)

	var names []string
	for _, h := range Providers[testRole](a) {
		names = append(names, h.HolderName())
	}
	assert.Equal(t, []string{"alpha", "gamma"}, names)
}

func TestCheckRoles_RefusesASingleRoleTwoPluginsImplement(t *testing.T) {
	a := startRolePlugins(t, &captchaHolder{fakePlugin: newFakePlugin("guard-b")}, &captchaHolder{fakePlugin: newFakePlugin("guard-a")})

	err := a.CheckRoles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "core.CaptchaMiddlewareProvider")
	assert.Contains(t, err.Error(), "(guard-a, guard-b)")
	assert.Nil(t, a.CaptchaMiddleware(context.Background()), "a role two plugins claim is wired from neither")
}

func TestCheckRoles_AcceptsOneOwnerAndManyContributors(t *testing.T) {
	a := startRolePlugins(t,
		&captchaHolder{fakePlugin: newFakePlugin("guard")},
		&preAuthHolder{newFakePlugin("resolver-a")},
		&preAuthHolder{newFakePlugin("resolver-b")},
	)

	assert.NoError(t, a.CheckRoles(), "a shared role may have any number of implementers")
}

// The accessor hands back the owner's own middleware, whatever the owner is
// called, rather than reading a plugin by a fixed name.
func TestCaptchaMiddleware_ComesFromThePluginThatImplementsIt(t *testing.T) {
	holder := &captchaHolder{fakePlugin: newFakePlugin("challenge")}
	a := startRolePlugins(t, holder)

	mw := a.CaptchaMiddleware(context.Background())
	require.NotNil(t, mw)
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", nil))
	assert.Equal(t, 1, holder.seen)
}

func TestWiredRoles_EachNamesItsInterfaceOnce(t *testing.T) {
	roles := WiredRoles()
	require.NotEmpty(t, roles)
	seen := map[string]bool{}
	for _, r := range roles {
		require.Equal(t, reflect.Interface, r.Type.Kind(), "%s", r.Name)
		assert.Equal(t, r.Type.String(), r.Name)
		assert.False(t, seen[r.Name], "%s is listed twice", r.Name)
		seen[r.Name] = true
	}

	roles[0].Name = "changed"
	assert.NotEqual(t, "changed", WiredRoles()[0].Name, "the caller gets a copy")
}
