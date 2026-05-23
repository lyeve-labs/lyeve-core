package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// servesOK answers 200 with no body, for a declaration whose handler does not
// matter to the test.
func servesOK() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func publicRoute(method, pattern string, rl *core.RouteRateLimit) plugin.RouteDecl {
	return plugin.RouteDecl{Method: method, Pattern: pattern, Group: plugin.GroupPublic, Handler: servesOK(), RateLimit: rl}
}

func tableFor(opts ...RouterOption) map[string]apimw.PublicRateLimitConfig {
	configs, _ := publicRateLimitTable(applyRouterOptions(opts))
	return configs
}

// The engine keeps a public rate row only for a route it serves itself. Any
// other route states its own limit, so a row the engine kept for it would
// pace the route in a way its owner never declared.
func TestDefaultPublicRateLimits_NameOnlyRoutesTheEngineServes(t *testing.T) {
	engine := engineOwnedRoutes("/api/admin")
	for k, v := range engineOwnedRoutes("/api/v1") {
		engine[k] = v
	}
	rows, _ := apimw.DefaultPublicRateLimits()
	require.NotEmpty(t, rows)
	for key := range rows {
		method, pattern, _ := strings.Cut(key, ":")
		_, served := engine[method+" "+normalizeRoutePattern(pattern)]
		assert.True(t, served, "the engine meters %s but does not serve it", key)
	}
}

// A route that states its own limit is held to it, not to a row the engine
// keeps for the same method and pattern.
func TestRouteRateLimit_DeclarationReplacesTheShippedRow(t *testing.T) {
	const key = "POST:/api/v1/probe/ping"
	got, _ := publicRateLimitTable(&routerOptions{
		publicRateLimits: map[string]apimw.PublicRateLimitConfig{key: {Rate: 5, Burst: 10}},
		pluginRoutes: []plugin.PluginRoutes{{Name: "probe", Routes: []plugin.RouteDecl{
			publicRoute(http.MethodPost, "/api/v1/probe/ping", &core.RouteRateLimit{Rate: 2, Burst: 4}),
		}}},
	})
	assert.Equal(t, apimw.PublicRateLimitConfig{Rate: 2, Burst: 4}, got[key])
}

// A route the engine ships no row for gets one from its declaration, so it is
// paced below the global cap.
func TestRouteRateLimit_DeclarationCoversARouteTheEngineDoesNotList(t *testing.T) {
	const key = "POST:/api/v1/probe/ping"
	shipped, _ := apimw.DefaultPublicRateLimits()
	require.NotContains(t, shipped, key)

	got := tableFor(WithPluginRoutes([]plugin.PluginRoutes{{Name: "probe", Routes: []plugin.RouteDecl{
		publicRoute(http.MethodPost, "/api/v1/probe/ping", &core.RouteRateLimit{Rate: 1, Burst: 3}),
	}}}))
	assert.Equal(t, apimw.PublicRateLimitConfig{Rate: 1, Burst: 3}, got[key])
}

// The operator keeps the last word: PUBLIC_RATE_LIMITS overrides a route's
// declaration as it overrides a shipped row.
func TestRouteRateLimit_OperatorOverrideWinsOverTheDeclaration(t *testing.T) {
	const key = "POST:/api/v1/probe/ping"
	overrides, _, err := apimw.ParsePublicRateLimits([]string{key + "=40:80"}, "")
	require.NoError(t, err)

	got := tableFor(
		WithPluginRoutes([]plugin.PluginRoutes{{Name: "probe", Routes: []plugin.RouteDecl{
			publicRoute(http.MethodPost, "/api/v1/probe/ping", &core.RouteRateLimit{Rate: 1, Burst: 3}),
		}}}),
		WithPublicRateLimitOverrides(overrides, nil),
	)
	assert.Equal(t, apimw.PublicRateLimitConfig{Rate: 40, Burst: 80}, got[key])
}

// The limiter wraps public routes only, and a bucket with no rate or no burst
// refuses traffic instead of pacing it. Neither declaration reaches the table,
// so the route keeps whatever the engine ships for it.
func TestRouteRateLimit_UnappliedDeclarationsLeaveTheTableAlone(t *testing.T) {
	admin := publicRoute(http.MethodGet, "/api/admin/probe", &core.RouteRateLimit{Rate: 1, Burst: 1})
	admin.Group = plugin.GroupAdmin
	got := tableFor(WithPluginRoutes([]plugin.PluginRoutes{{Name: "probe", Routes: []plugin.RouteDecl{
		admin,
		publicRoute(http.MethodPost, "/api/admin/auth/login", &core.RouteRateLimit{Rate: 0, Burst: 4}),
		publicRoute(http.MethodPost, "/api/v1/probe/ping", &core.RouteRateLimit{Rate: 1, Burst: 0}),
	}}}))

	shipped, _ := apimw.DefaultPublicRateLimits()
	assert.Equal(t, shipped, got, "no declaration here is one the limiter applies")
}

// The declaration is keyed the way the limiter wraps a mounted route, so the
// route is paced by it: a burst of three admits three.
func TestRouteRateLimit_MountedRouteAdmitsItsDeclaredBurst(t *testing.T) {
	routes := []plugin.PluginRoutes{{Name: "probe", Routes: []plugin.RouteDecl{
		publicRoute(http.MethodPost, "/api/v1/probe/ping", &core.RouteRateLimit{Rate: 0.001, Burst: 3}),
	}}}
	l := limiterFor(t, WithPluginRoutes(routes))
	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/v1", false, 1<<20, l, nil, nil)

	admittedCount := 0
	for range 10 {
		req := httptest.NewRequest(http.MethodPost, "/probe/ping", nil)
		req.RemoteAddr = "198.51.100.9:54321"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			admittedCount++
		}
	}
	assert.Equal(t, 3, admittedCount)
	require.NoError(t, l.Verify(), "a declared limit fills the per-route gap the verifier reports")
}

// A declaration the limiter skips is named at boot, so the route's author
// learns the limit is not the one in force.
func TestWarnUnmountableRoutes_NamesARateLimitThatIsNotApplied(t *testing.T) {
	admin := publicRoute(http.MethodGet, "/api/admin/probe", &core.RouteRateLimit{Rate: 1, Burst: 1})
	admin.Group = plugin.GroupAdmin
	out := captureWarnings(t, func() {
		WarnUnmountableRoutes(routesOf(
			admin,
			publicRoute(http.MethodPost, "/api/v1/probe/zero", &core.RouteRateLimit{Rate: 0, Burst: 4}),
		))
	})
	assert.Contains(t, out, "/api/admin/probe")
	assert.Contains(t, out, "/api/v1/probe/zero")
	assert.Contains(t, out, "rate limit the public limiter will not apply")

	quiet := captureWarnings(t, func() {
		WarnUnmountableRoutes(routesOf(publicRoute(http.MethodPost, "/api/v1/probe/ping", &core.RouteRateLimit{Rate: 1, Burst: 3})))
	})
	assert.Empty(t, quiet)
}
