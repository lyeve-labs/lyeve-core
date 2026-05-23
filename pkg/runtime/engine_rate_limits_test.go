package runtime

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

func findEngineLimit(limits []core.EngineRateLimit, scope, endpoint string) (core.EngineRateLimit, bool) {
	for _, l := range limits {
		if l.Scope == scope && l.Endpoint == endpoint {
			return l, true
		}
	}
	return core.EngineRateLimit{}, false
}

func TestEngineRateLimits_DefaultsWithoutSettings(t *testing.T) {
	limits := engineRateLimits(0, 0, false, nil, "", nil)

	_, hasGlobal := findEngineLimit(limits, "global", "*")
	assert.False(t, hasGlobal, "no global cap without RATE_LIMIT_RPS")

	login, ok := findEngineLimit(limits, "public", "POST /api/admin/auth/login")
	require.True(t, ok)
	assert.Equal(t, "default", login.Source)
	assert.Equal(t, "PUBLIC_RATE_LIMITS", login.Setting)

	cap, ok := findEngineLimit(limits, "public", "*")
	require.True(t, ok)
	assert.Equal(t, "default", cap.Source)
}

func TestEngineRateLimits_OverridesAreMarked(t *testing.T) {
	limits := engineRateLimits(20, 40, true,
		[]string{"POST:/api/admin/auth/login=50:100"}, "200:400", nil)

	global, ok := findEngineLimit(limits, "global", "*")
	require.True(t, ok)
	assert.Equal(t, 20.0, global.Rate)
	assert.True(t, global.PerTenant)

	login, ok := findEngineLimit(limits, "public", "POST /api/admin/auth/login")
	require.True(t, ok)
	assert.Equal(t, "environment", login.Source)
	assert.Equal(t, 50.0, login.Rate)
	assert.Equal(t, 100, login.Burst)

	token, ok := findEngineLimit(limits, "public", "POST /api/v1/auth/token")
	require.True(t, ok)
	assert.Equal(t, "default", token.Source, "an override on one route leaves the others at their defaults")

	cap, ok := findEngineLimit(limits, "public", "*")
	require.True(t, ok)
	assert.Equal(t, "environment", cap.Source)
	assert.Equal(t, 200.0, cap.Rate)
}

// A limit a route declares is in the report as the default it ships with,
// between the engine's own rows and the operator's overrides: it takes the
// place of an engine row for the same route, and PUBLIC_RATE_LIMITS still
// moves it.
func TestEngineRateLimits_ReportsTheLimitsRoutesDeclare(t *testing.T) {
	declared := map[string]apimw.PublicRateLimitConfig{
		"POST:/api/v1/probe/ping":    {Rate: 2, Burst: 4},
		"POST:/api/v1/probe/pong":    {Rate: 3, Burst: 6},
		"POST:/api/admin/auth/login": {Rate: 1, Burst: 2},
	}
	limits := engineRateLimits(0, 0, false, []string{"POST:/api/v1/probe/pong=30:60"}, "", declared)

	ping, ok := findEngineLimit(limits, "public", "POST /api/v1/probe/ping")
	require.True(t, ok, "a declared limit is reported")
	assert.Equal(t, core.EngineRateLimit{Scope: "public", Endpoint: "POST /api/v1/probe/ping", Rate: 2, Burst: 4, Source: "default", Setting: "PUBLIC_RATE_LIMITS"}, ping)

	pong, ok := findEngineLimit(limits, "public", "POST /api/v1/probe/pong")
	require.True(t, ok)
	assert.Equal(t, "environment", pong.Source, "the operator's override wins over the declaration")
	assert.Equal(t, 30.0, pong.Rate)
	assert.Equal(t, 60, pong.Burst)

	login, ok := findEngineLimit(limits, "public", "POST /api/admin/auth/login")
	require.True(t, ok)
	assert.Equal(t, 1.0, login.Rate, "a declaration takes the place of the engine's row for its route")
	assert.Equal(t, "default", login.Source)

	without := engineRateLimits(0, 0, false, nil, "", nil)
	assert.Len(t, limits, len(without)+2, "each declared route adds one row, and the one the engine lists replaces its row")
}

// The report is taken from the routes a router build mounts, with the limits
// the routers read from them: a limit on a route outside the public group,
// or one with no rate, is not one the limiter applies, and it is not
// reported.
func TestPublishEngineRateLimits_ReportsWhatTheMountedRoutesDeclare(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	routes := []plugin.PluginRoutes{{Name: "probe", Routes: []plugin.RouteDecl{
		{Method: http.MethodPost, Pattern: "/api/v1/probe/ping", Group: plugin.GroupPublic, Handler: ok, RateLimit: &core.RouteRateLimit{Rate: 2, Burst: 4}},
		{Method: http.MethodPost, Pattern: "/api/admin/probe/admin", Group: plugin.GroupAdmin, Handler: ok, RateLimit: &core.RouteRateLimit{Rate: 2, Burst: 4}},
		{Method: http.MethodPost, Pattern: "/api/v1/probe/zero", Group: plugin.GroupPublic, Handler: ok, RateLimit: &core.RouteRateLimit{Rate: 0, Burst: 4}},
	}}}
	require.Len(t, api.DeclaredPublicRateLimits(routes), 1, "the routers apply one of the three")

	host := &rateLimitRecorder{}
	publishEngineRateLimits(host, &config.Config{}, routes)
	_, found := findEngineLimit(host.limits, "public", "POST /api/v1/probe/ping")
	assert.True(t, found)
	for _, endpoint := range []string{"POST /api/admin/probe/admin", "POST /api/v1/probe/zero"} {
		_, found := findEngineLimit(host.limits, "public", endpoint)
		assert.False(t, found, "%s is not a limit the public limiter applies", endpoint)
	}
	assert.Len(t, host.limits, len(engineRateLimits(0, 0, false, nil, "", nil))+1)

	publishEngineRateLimits(host, &config.Config{}, nil)
	_, found = findEngineLimit(host.limits, "public", "POST /api/v1/probe/ping")
	assert.False(t, found, "a later build replaces the report, so a route that is gone leaves it")
}

// rateLimitRecorder is a host that keeps the last report handed to it.
type rateLimitRecorder struct {
	core.Host
	limits []core.EngineRateLimit
}

func (h *rateLimitRecorder) WithEngineRateLimits(limits []core.EngineRateLimit) { h.limits = limits }
