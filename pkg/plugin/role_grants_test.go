package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The permissions plugin declares its reach through RegisterPluginWithCaps,
// and that declaration was written before the checker role had a capability.
const permissionsDeclaration = core.CapDBRead | core.CapDBWrite | core.CapRawDB | core.CapRoutes

func TestCapPolicy_ADeclarationKeepsTheRoleTheTableGives(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	restoreCapTable(t)

	SetCapPolicy(map[string]core.Capability{
		"permissions": core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapHooks | core.CapPermissionChecker,
	})
	p := &hostKeeper{name: "permissions"}
	RegisterPluginWithCaps(p.name, func() core.Plugin { return p }, permissionsDeclaration)

	got := CapPolicy(p.name)
	assert.True(t, got.Has(core.CapPermissionChecker), "the declaration took the checker role from its plugin")
	assert.False(t, got.Has(core.CapHooks), "a declaration still narrows what the plugin reaches")
}

func TestCapPolicy_ADeclarationCannotClaimARoleTheTableWithholds(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	restoreCapTable(t)

	SetCapPolicy(map[string]core.Capability{"ab-testing": core.CapDBWrite | core.CapRawDB | core.CapRoutes})
	p := &hostKeeper{name: "ab-testing"}
	RegisterPluginWithCaps(p.name, func() core.Plugin { return p }, core.CapDBWrite|core.CapRoles)

	assert.Zero(t, CapPolicy(p.name)&core.CapRoles, "a plugin granted itself a role")
}

// checkerPlugin registers a permission checker in Start and reports the
// registrar's answer as its own, the way a plugin that cannot serve its role
// should.
type checkerPlugin struct {
	name    string
	checker core.PermissionChecker
}

func (p *checkerPlugin) Name() string { return p.name }

func (p *checkerPlugin) Start(_ context.Context, host core.Host) error {
	reg, ok := host.(core.PermissionCheckerRegistrar)
	if !ok {
		return errors.New("the host takes no permission checker")
	}
	return reg.RegisterPermissionChecker(p.checker)
}

func (p *checkerPlugin) Stop(context.Context) error { return nil }

type fixedChecker struct{ allow bool }

func (c *fixedChecker) Allowed(context.Context, []string, string, string) (bool, error) {
	return c.allow, nil
}

// checkerHost takes a permission checker and keeps the record of holders, as
// the engine host does.
type checkerHost struct {
	testHost
	core.HostRoleRegistry
	mu      sync.Mutex
	checker core.PermissionChecker
}

func (h *checkerHost) RegisterPermissionChecker(c core.PermissionChecker) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checker = c
	return nil
}

func (h *checkerHost) installed() core.PermissionChecker {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checker
}

// Through the activator, with a table the way a build hands it over: the
// plugin the table gives the checker to registers it, and a plugin without
// the grant sees the refusal in its own Start and fails there, leaving the
// rules in place.
func TestActivator_ARefusedRoleRegistrationFailsThePluginsStart(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	restoreCapTable(t)

	SetCapPolicy(map[string]core.Capability{
		"permissions": core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapPermissionChecker,
		"ab-testing":  core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	})
	rules := &fixedChecker{}
	RegisterPluginWithCaps("permissions", func() core.Plugin {
		return &checkerPlugin{name: "permissions", checker: rules}
	}, permissionsDeclaration)
	RegisterPlugin("ab-testing", func() core.Plugin {
		return &checkerPlugin{name: "ab-testing", checker: &fixedChecker{allow: true}}
	})

	host := &checkerHost{}
	a := NewActivator(host, silentLogger)
	a.Resolve(grants("permissions", "ab-testing"), "")
	require.NoError(t, a.Start(context.Background()))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	status := map[string]PluginStatus{}
	for _, s := range a.Status().Plugins {
		status[s.Name] = s
	}
	assert.Equal(t, PhaseRunning, status["permissions"].Phase, status["permissions"].LastError)
	assert.Equal(t, PhaseFailed, status["ab-testing"].Phase)
	assert.Contains(t, status["ab-testing"].LastError, core.ErrCapDenied.Error())
	assert.Same(t, rules, host.installed())
}
