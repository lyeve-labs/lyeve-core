package runtime

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest/mockhost"
)

// The plugin middleware a request crosses between tenancy and the routes is
// assembled from slots, and where a slot sits decides what it sees. The quota
// gate must not count a request a limiter refused, the sampler times what is
// mounted below it, and masking has to see the body the handler wrote. These
// tests hold both routers to the order a request crosses the slots in.
//
// The idempotency slot is mounted by the router itself, after the chain and
// before the routes, so a request reaches it last whatever position the
// runtime hands its option in.

var (
	wantAdminChain = []string{
		"brute-force", "captcha", "rate-limit", "monitor", "sampler",
		"analytics", "waf", "pii", "usage", "idempotency",
	}
	wantAPIChain = []string{
		"brute-force", "captcha", "rate-limit", "monitor", "sampler",
		"analytics", "waf", "pii", "usage", "residency", "idempotency",
	}
)

// chainTrace records the slots a request crossed, in the order it crossed
// them.
type chainTrace struct {
	mu    sync.Mutex
	slots []string
}

func (c *chainTrace) record(slot string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slots = append(c.slots, slot)
}

func (c *chainTrace) mark(slot string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.record(slot)
			next.ServeHTTP(w, r)
		})
	}
}

// take returns what was recorded and starts the next request from nothing.
func (c *chainTrace) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.slots
	c.slots = nil
	return out
}

// slotPlugin is a plugin that does nothing but fill one slot.
type slotPlugin struct{ name string }

func (p *slotPlugin) Name() string                           { return p.name }
func (p *slotPlugin) Start(context.Context, core.Host) error { return nil }
func (p *slotPlugin) Stop(context.Context) error             { return nil }

type bruteForceSlot struct {
	slotPlugin
	mw func(http.Handler) http.Handler
}

func (p *bruteForceSlot) BruteForceMiddleware() func(http.Handler) http.Handler { return p.mw }

type captchaSlot struct {
	slotPlugin
	mw func(http.Handler) http.Handler
}

func (p *captchaSlot) CaptchaMiddleware(context.Context) func(http.Handler) http.Handler {
	return p.mw
}

// idempotencySlot also offers generic middleware, as a plugin that holds this
// slot may. Only its own slot may mount it, so a chain that picked up every
// generic middleware would show it twice.
type idempotencySlot struct {
	slotPlugin
	mw, generic func(http.Handler) http.Handler
}

func (p *idempotencySlot) IdempotencyMiddleware() func(http.Handler) http.Handler { return p.mw }
func (p *idempotencySlot) Middleware() func(http.Handler) http.Handler            { return p.generic }

type samplerSlot struct {
	slotPlugin
	mw func(http.Handler) http.Handler
}

func (p *samplerSlot) RequestSampler() func(http.Handler) http.Handler { return p.mw }

type maskingSlot struct {
	slotPlugin
	mw func(http.Handler) http.Handler
}

func (p *maskingSlot) PIIMiddleware() func(http.Handler) http.Handler { return p.mw }

// phasedSlot contributes its entries through core.ChainMiddlewareProvider.
type phasedSlot struct {
	slotPlugin
	entries []core.ChainEntry
}

func (p *phasedSlot) ChainMiddleware() []core.ChainEntry { return p.entries }

// roleSlotPlugins fills the slots that have an interface of their own.
func roleSlotPlugins(trace *chainTrace) []core.Plugin {
	return []core.Plugin{
		&bruteForceSlot{slotPlugin{"login-guard"}, trace.mark("brute-force")},
		&captchaSlot{slotPlugin{"challenge"}, trace.mark("captcha")},
		&idempotencySlot{slotPlugin{"dedupe"}, trace.mark("idempotency"), trace.mark("idempotency-generic")},
		&samplerSlot{slotPlugin{"sampler"}, trace.mark("sampler")},
		&maskingSlot{slotPlugin{"masker"}, trace.mark("pii")},
	}
}

// residencyGuard stands in for a write guard at the residency phase, which
// looks up a region only for a write that names a tenant. It records when it
// would, so the API router is held to handing that phase the tenant the
// request resolved to.
func residencyGuard(trace *chainTrace) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && core.TenantIDFromCtx(r.Context()) != "" {
				trace.record("residency")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// residencyAt is a plugin that guards writes at the residency phase, on the
// API router only.
func residencyAt(trace *chainTrace, name string) core.Plugin {
	return &phasedSlot{slotPlugin{name}, []core.ChainEntry{
		{Phase: core.ChainPhaseResidency, Routers: core.ChainAPI, Middleware: residencyGuard(trace)},
	}}
}

// phasedAt is a plugin that declares one chain entry at phase.
func phasedAt(trace *chainTrace, name, label string, phase core.ChainPhase) core.Plugin {
	return &phasedSlot{slotPlugin{name}, []core.ChainEntry{
		{Phase: phase, Routers: core.ChainBoth, Middleware: trace.mark(label)},
	}}
}

// slotPlugins fills every slot: the slots with an interface of their own, and
// the phased entries.
func slotPlugins(trace *chainTrace) []core.Plugin {
	return append(roleSlotPlugins(trace),
		phasedAt(trace, "limiter", "rate-limit", core.ChainPhaseRateLimit),
		phasedAt(trace, "monitor", "monitor", core.ChainPhaseMonitor),
		phasedAt(trace, "counter", "analytics", core.ChainPhaseAnalytics),
		phasedAt(trace, "firewall", "waf", core.ChainPhaseFirewall),
		phasedAt(trace, "meter", "usage", core.ChainPhaseQuota),
		residencyAt(trace, "region-guard"),
	)
}

// phasedSlotPlugins fills the same slots from plugins registered under other
// names, so the order cannot depend on what a plugin is called.
func phasedSlotPlugins(trace *chainTrace) []core.Plugin {
	return append(roleSlotPlugins(trace),
		phasedAt(trace, "chain-limit", "rate-limit", core.ChainPhaseRateLimit),
		phasedAt(trace, "chain-monitor", "monitor", core.ChainPhaseMonitor),
		phasedAt(trace, "chain-analytics", "analytics", core.ChainPhaseAnalytics),
		phasedAt(trace, "chain-firewall", "waf", core.ChainPhaseFirewall),
		phasedAt(trace, "chain-quota", "usage", core.ChainPhaseQuota),
		residencyAt(trace, "chain-residency"),
	)
}

// grantNamed is a policy that starts the plugins it names and refuses the
// rest, the way a licensing implementation's manager answers the activator.
type grantNamed map[string]bool

func (p grantNamed) Plugin(name string) licensing.PluginGrant {
	return licensing.PluginGrant{Start: p[name]}
}

// granting is the policy that starts exactly names.
func granting(names []string) grantNamed {
	p := grantNamed{}
	for _, n := range names {
		p[n] = true
	}
	return p
}

// startSlotPlugins registers plugins, grants them, and starts them the way
// the runtime does.
func startSlotPlugins(t *testing.T, plugins []core.Plugin) *plugin.Activator {
	t.Helper()
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		plugin.RegisterPlugin(p.Name(), func() core.Plugin { return p })
		t.Cleanup(func() { plugin.UnregisterPlugin(p.Name()) })
		names = append(names, p.Name())
	}
	a := plugin.NewActivator(mockhost.New(t).Host(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.Resolve(granting(names), "")
	require.NoError(t, a.Start(context.Background()))
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	for _, s := range a.Status().Plugins {
		require.True(t, s.Active, "%s did not start: %s %s", s.Name, s.Reason, s.LastError)
	}
	return a
}

// chainConfig is the smallest configuration both routers build from, with a
// region set as on an instance that runs a residency guard.
func chainConfig() *config.Config {
	return &config.Config{
		DatabaseDriver:   "postgres",
		JWTSecret:        "test-secret-for-the-plugin-chain-order",
		JWTSecrets:       []string{"test-secret-for-the-plugin-chain-order"},
		JWTExpirySecs:    3600,
		MaxBodyBytes:     10 << 20,
		MaxJSONBodyBytes: 1 << 20,
		InstanceRegion:   "eu-west",
	}
}

// crossedChains builds both routers the way the runtime does, sends each one
// request, and returns the slots each request crossed.
func crossedChains(t *testing.T, a *plugin.Activator, trace *chainTrace) (admin, apiChain []string) {
	t.Helper()
	pool := testdb.Postgres(t)
	cfg := chainConfig()
	ctx := context.Background()
	lifetime, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	latency := apimw.NewLatencyTracker(200, 500)

	adminOpts := buildAdminExtraMW(ctx, cfg, a, NewInflightDrainer(), apimw.BackpressureResult{}, latency)
	adminRouter, err := api.NewAdminRouter(pool, cfg, append(adminOpts, api.WithLifetime(lifetime))...)
	require.NoError(t, err)
	trace.take()
	adminRouter.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/admin/chain-probe", nil))
	admin = trace.take()

	apiOpts := buildAPIExtraMW(ctx, cfg, a, NewInflightDrainer(), apimw.BackpressureResult{}, latency)
	apiRouter, err := api.NewAPIRouter(pool, cfg, nil, append(apiOpts, api.WithLifetime(lifetime))...)
	require.NoError(t, err)
	// A write with a session, because a residency guard looks up the tenant's
	// region only on a write that names a tenant.
	token, err := auth.Sign(cfg.JWTSecret, 3600, uuid.New(), "chain@test.com", []string{"editor"}, "", 1)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chain-probe", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	trace.take()
	apiRouter.ServeHTTP(httptest.NewRecorder(), req)
	apiChain = trace.take()
	return admin, apiChain
}

func TestPluginChain_EachRouterCrossesTheSlotsInOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("builds both routers on a database")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	trace := &chainTrace{}
	a := startSlotPlugins(t, slotPlugins(trace))

	admin, apiChain := crossedChains(t, a, trace)
	require.Equal(t, wantAdminChain, admin, "the admin router")
	require.Equal(t, wantAPIChain, apiChain, "the API router")
}

// Plugins that declare the same phases under other names put a request
// through the same order.
func TestPluginChain_DeclaredPhasesKeepTheOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("builds both routers on a database")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	trace := &chainTrace{}
	a := startSlotPlugins(t, phasedSlotPlugins(trace))

	admin, apiChain := crossedChains(t, a, trace)
	require.Equal(t, wantAdminChain, admin, "the admin router")
	require.Equal(t, wantAPIChain, apiChain, "the API router")
}

// An entry between two phases, or for one router, mounts where its phase and
// routers say, among the engine's own slots.
func TestPluginChain_AnEntryMountsWhereItsPhaseSays(t *testing.T) {
	if testing.Short() {
		t.Skip("builds both routers on a database")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	trace := &chainTrace{}
	extra := &phasedSlot{slotPlugin{"chain-extra"}, []core.ChainEntry{
		{Phase: core.ChainPhaseMonitor + 50, Routers: core.ChainAdmin, Middleware: trace.mark("between")},
		{Phase: core.ChainPhaseResidency + 50, Routers: core.ChainAPI, Middleware: trace.mark("region-guard")},
	}}
	a := startSlotPlugins(t, append(slotPlugins(trace), extra))

	admin, apiChain := crossedChains(t, a, trace)
	require.Equal(t, []string{
		"brute-force", "captcha", "rate-limit", "monitor", "between", "sampler",
		"analytics", "waf", "pii", "usage", "idempotency",
	}, admin, "the admin router")
	require.Equal(t, []string{
		"brute-force", "captcha", "rate-limit", "monitor", "sampler",
		"analytics", "waf", "pii", "usage", "residency", "region-guard", "idempotency",
	}, apiChain, "the API router")
}
