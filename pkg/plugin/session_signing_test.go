package plugin

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The oauth plugin declares its reach through RegisterPluginWithCaps, and that
// declaration was written before signing a session had a capability.
const oauthDeclaration = core.CapDBRead | core.CapDBWrite | core.CapRawDB | core.CapConfigSecret | core.CapRoutes

func TestCapPolicy_ADeclarationKeepsTheSessionSigningTheTableGives(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	restoreCapTable(t)

	SetCapPolicy(map[string]core.Capability{
		"oauth": core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret | core.CapHooks | core.CapSessionSign,
	})
	p := &hostKeeper{name: "oauth"}
	RegisterPluginWithCaps(p.name, func() core.Plugin { return p }, oauthDeclaration)

	got := CapPolicy(p.name)
	assert.True(t, got.Has(core.CapSessionSign), "the declaration took sign-in from its plugin")
	assert.False(t, got.Has(core.CapHooks), "a declaration still narrows what the plugin reaches")
}

func TestCapPolicy_ADeclarationCannotClaimSessionSigningTheTableWithholds(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	restoreCapTable(t)

	SetCapPolicy(map[string]core.Capability{"webhook": core.CapDBWrite | core.CapRawDB | core.CapRoutes})
	p := &hostKeeper{name: "webhook"}
	RegisterPluginWithCaps(p.name, func() core.Plugin { return p }, core.CapDBWrite|core.CapSessionSign)

	assert.False(t, CapPolicy(p.name).Has(core.CapSessionSign), "a plugin granted itself session signing")
}

func TestCapPolicy_APlainRegistrationGetsTheSessionSigningTheTableGives(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	restoreCapTable(t)

	SetCapPolicy(map[string]core.Capability{"mfa": core.CapDBWrite | core.CapRoutes | core.CapSessionSign})
	p := &hostKeeper{name: "mfa"}
	RegisterPlugin(p.name, func() core.Plugin { return p })

	assert.True(t, CapPolicy(p.name).Has(core.CapSessionSign), "a plain registration lost sign-in")
}
