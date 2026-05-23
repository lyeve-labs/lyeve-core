package plugin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// chainPlugin contributes the entries it is given through
// core.ChainMiddlewareProvider.
type chainPlugin struct {
	*fakePlugin
	entries []core.ChainEntry
}

func (p *chainPlugin) ChainMiddleware() []core.ChainEntry { return p.entries }

// genericPlugin offers only core.MiddlewareProvider.
type genericPlugin struct {
	*fakePlugin
	mw func(http.Handler) http.Handler
}

func (p *genericPlugin) Middleware() func(http.Handler) http.Handler { return p.mw }

// dualPlugin offers both shapes.
type dualPlugin struct {
	chainPlugin
	generic func(http.Handler) http.Handler
}

func (p *dualPlugin) Middleware() func(http.Handler) http.Handler { return p.generic }

// crossed sends one request through the entries and returns the labels in
// the order the request met them.
func crossed(t *testing.T, entries []core.ChainEntry, order *[]string) []string {
	t.Helper()
	*order = nil
	var h http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for i := len(entries) - 1; i >= 0; i-- {
		h = entries[i].Middleware(h)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	return *order
}

func TestChainMiddleware_OrdersByPhaseThenPluginNameThenListing(t *testing.T) {
	var order []string
	a := startRolePlugins(t,
		&chainPlugin{newFakePlugin("zeta"), []core.ChainEntry{
			{Phase: core.ChainPhaseFirewall, Routers: core.ChainBoth, Middleware: tagMiddleware(&order, "zeta-firewall")},
			{Phase: core.ChainPhaseRateLimit, Routers: core.ChainBoth, Middleware: tagMiddleware(&order, "zeta-limit")},
		}},
		&chainPlugin{newFakePlugin("alpha"), []core.ChainEntry{
			{Phase: core.ChainPhaseFirewall, Routers: core.ChainBoth, Middleware: tagMiddleware(&order, "alpha-firewall-1")},
			{Phase: core.ChainPhaseFirewall, Routers: core.ChainBoth, Middleware: tagMiddleware(&order, "alpha-firewall-2")},
			{Phase: core.ChainPhaseMonitor + 50, Routers: core.ChainBoth, Middleware: tagMiddleware(&order, "alpha-between")},
		}},
	)

	got := crossed(t, a.ChainMiddleware(core.ChainAdmin), &order)
	assert.Equal(t, []string{"zeta-limit", "alpha-between", "alpha-firewall-1", "alpha-firewall-2", "zeta-firewall"}, got)
}

func TestChainMiddleware_AnEntryMountsOnlyOnItsRouters(t *testing.T) {
	var order []string
	a := startRolePlugins(t,
		&chainPlugin{newFakePlugin("split"), []core.ChainEntry{
			{Phase: core.ChainPhaseQuota, Routers: core.ChainAdmin, Middleware: tagMiddleware(&order, "admin")},
			{Phase: core.ChainPhaseQuota, Routers: core.ChainAPI, Middleware: tagMiddleware(&order, "api")},
			{Phase: core.ChainPhaseQuota, Routers: core.ChainBoth, Middleware: tagMiddleware(&order, "both")},
			{Phase: core.ChainPhaseQuota, Routers: 0, Middleware: tagMiddleware(&order, "nowhere")},
			{Phase: core.ChainPhaseQuota, Routers: core.ChainBoth},
		}},
	)

	assert.Equal(t, []string{"admin", "both"}, crossed(t, a.ChainMiddleware(core.ChainAdmin), &order))
	assert.Equal(t, []string{"api", "both"}, crossed(t, a.ChainMiddleware(core.ChainAPI), &order))
}

// A plugin's name places nothing in the chain. A plugin that offers only the
// generic shape mounts on neither router, whatever it is named.
func TestChainMiddleware_ANameAloneMountsNothing(t *testing.T) {
	var order []string
	a := startRolePlugins(t,
		&genericPlugin{newFakePlugin("widgets"), tagMiddleware(&order, "widgets")},
		&genericPlugin{newFakePlugin("gadgets"), tagMiddleware(&order, "gadgets")},
		&genericPlugin{newFakePlugin("example"), tagMiddleware(&order, "example")},
		&genericPlugin{newFakePlugin("sample"), tagMiddleware(&order, "sample")},
		&genericPlugin{newFakePlugin("probe"), tagMiddleware(&order, "probe")},
	)
	require.Len(t, Providers[core.MiddlewareProvider](a), 5, "every plugin runs and offers the generic shape")

	assert.Empty(t, a.ChainMiddleware(core.ChainAdmin))
	assert.Empty(t, a.ChainMiddleware(core.ChainAPI))
}

// The generic shape places nothing on its own, because a plugin may implement
// it for another purpose.
func TestChainMiddleware_GenericMiddlewareIsNotMounted(t *testing.T) {
	var order []string
	a := startRolePlugins(t, &genericPlugin{newFakePlugin("replay-guard"), tagMiddleware(&order, "replay-guard")})

	assert.Empty(t, a.ChainMiddleware(core.ChainBoth))
}

// A plugin that declares its phases is placed by them, and its generic
// middleware is not mounted as well.
func TestChainMiddleware_DeclaredPhasesWinOverGenericMiddleware(t *testing.T) {
	var order []string
	a := startRolePlugins(t, &dualPlugin{
		chainPlugin: chainPlugin{newFakePlugin("waf"), []core.ChainEntry{
			{Phase: core.ChainPhaseQuota, Routers: core.ChainBoth, Middleware: tagMiddleware(&order, "declared")},
		}},
		generic: tagMiddleware(&order, "generic"),
	})

	entries := a.ChainMiddleware(core.ChainBoth)
	require.Len(t, entries, 1)
	assert.Equal(t, core.ChainPhaseQuota, entries[0].Phase)
	assert.Equal(t, []string{"declared"}, crossed(t, entries, &order))
}
