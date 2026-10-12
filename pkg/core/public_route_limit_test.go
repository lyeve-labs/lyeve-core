package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type routeLimits map[string]RouteRateLimit

func (m routeLimits) PublicRouteLimit(endpoint string) (RouteRateLimit, bool) {
	l, ok := m[endpoint]
	return l, ok
}

func TestResolvePublicRouteLimit_AnswersTheRegisteredResolver(t *testing.T) {
	unregister := RegisterPublicRouteLimitResolver(routeLimits{
		"GET /api/v1/things":   {Rate: 200, Burst: 400},
		"GET /api/v1/zero":     {Rate: 0, Burst: 10},
		"GET /api/v1/no-burst": {Rate: 5, Burst: 0},
	})
	t.Cleanup(unregister)

	got, ok := ResolvePublicRouteLimit("GET /api/v1/things")
	assert.True(t, ok)
	assert.Equal(t, RouteRateLimit{Rate: 200, Burst: 400}, got)

	_, ok = ResolvePublicRouteLimit("GET /api/v1/other")
	assert.False(t, ok, "a route the resolver does not know runs on the general limit")
	_, ok = ResolvePublicRouteLimit("GET /api/v1/zero")
	assert.False(t, ok, "a zero rate would refuse every request, so it is not applied")
	_, ok = ResolvePublicRouteLimit("GET /api/v1/no-burst")
	assert.False(t, ok, "a zero burst would refuse every request, so it is not applied")
}

func TestResolvePublicRouteLimit_NothingRegisteredAnswersNothing(t *testing.T) {
	_, ok := ResolvePublicRouteLimit("GET /api/v1/things")
	assert.False(t, ok)
}

func TestRegisterPublicRouteLimitResolver_AReplacedResolverCannotRemoveItsSuccessor(t *testing.T) {
	first := RegisterPublicRouteLimitResolver(routeLimits{"GET /a": {Rate: 1, Burst: 1}})
	second := RegisterPublicRouteLimitResolver(routeLimits{"GET /a": {Rate: 2, Burst: 2}})
	t.Cleanup(second)

	first()
	got, ok := ResolvePublicRouteLimit("GET /a")
	assert.True(t, ok, "removing the replaced resolver leaves its successor in place")
	assert.Equal(t, 2.0, got.Rate)

	second()
	_, ok = ResolvePublicRouteLimit("GET /a")
	assert.False(t, ok)
}
