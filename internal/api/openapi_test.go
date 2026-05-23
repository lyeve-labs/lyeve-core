package api

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// OpenAPI spec ↔ route consistency tests
//
// These tests walk the chi routers and compare registered method+pattern
// pairs against the paths declared in buildOpenAPIDoc. Two directions:
//
//   - Spec->Router: every path in the spec must have a registered route.
//     Catches stale spec entries (route removed, spec not updated).
//   - Router->Spec: every registered route must appear in the spec.
//     Catches undocumented endpoints (route added, spec not updated).
//
// A plugin route is documented from its declaration and mounted from the
// same one, so the minimal test routers, which mount no plugin, have none on
// either side.

// collectRoutes walks a chi.Router (or http.Handler backed by chi) and
// returns a set of "METHOD /path" strings.
func collectRoutes(t *testing.T, h http.Handler) map[string]bool {
	t.Helper()
	routes := make(map[string]bool)
	err := chi.Walk(h.(chi.Routes), func(method string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes[method+" "+route] = true
		return nil
	})
	require.NoError(t, err, "chi.Walk failed")
	return routes
}

// debugPathPrefixes are internal/debug routes intentionally excluded from
// the OpenAPI spec (pprof, latency debug).
var debugPathPrefixes = []string{
	"/healthz",
	"/readyz",
	"/startup",
	"/api/admin/debug",
}

// k8sProbePrefixes are Kubernetes health probe paths registered on the
// outer router before the /api/admin or /api/v1 groups.
var k8sProbePrefixes = []string{
	"/healthz",
	"/readyz",
	"/startup",
}

// contentAPIPrefixes are dynamic content CRUD paths whose presence depends
// on runtime schema names. They're documented in the spec via the
// schemaNames loop but can't be verified statically.
var contentAPIPrefixes = []string{
	"/api/v1/content/",
}

func hasAnyPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// testConfig returns a minimal config that lets both routers construct
// without errors. No real DB is needed: stores are instantiated but
// never queried.
func testConfig() *config.Config {
	return &config.Config{
		DatabaseDriver:   "postgres",
		JWTSecret:        "test-secret-for-openapi-spec-validation",
		JWTSecrets:       []string{"test-secret-for-openapi-spec-validation"},
		JWTExpirySecs:    3600,
		MaxBodyBytes:     10 << 20,
		MaxJSONBodyBytes: 1 << 20,
		// Rate limiting and IP allowlist disabled to keep router simple.
	}
}

// buildTestRouters constructs both admin and API routers with minimal
// dependencies (no DB, no plugins). Returns the routers or fails the test.
func buildTestRouters(t *testing.T) (admin http.Handler, api http.Handler) {
	t.Helper()
	pool := &fakeDB{engine: "postgres"}
	cfg := testConfig()

	adminRouter, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
	require.NoError(t, err, "NewAdminRouter failed")

	apiRouter, err := NewAPIRouter(pool, cfg, nil, WithLifetime(testLifetime(t)))
	require.NoError(t, err, "NewAPIRouter failed")

	return adminRouter, apiRouter
}

// Test: Spec->Router, every OpenAPI path must have a registered route

func TestOpenAPISpecPathsHaveRegisteredRoutes(t *testing.T) {
	adminRouter, apiRouter := buildTestRouters(t)

	registered := collectRoutes(t, adminRouter)
	for k, v := range collectRoutes(t, apiRouter) {
		registered[k] = v
	}

	doc := buildOpenAPIDoc(nil, nil, openAPIOptions{}) // no dynamic schema names

	var mismatches []string
	for path, methods := range doc.Paths {
		// Skip the dynamic content paths.
		if hasAnyPrefix(path, contentAPIPrefixes) {
			continue
		}
		for method := range methods {
			key := strings.ToUpper(method) + " " + path
			if !registered[key] {
				mismatches = append(mismatches, key)
			}
		}
	}

	sort.Strings(mismatches)
	for _, m := range mismatches {
		t.Errorf("OpenAPI spec declares %s but no matching route is registered", m)
	}
}

// Test: Router->Spec, every registered route must be in the OpenAPI spec

// Registered routes the document may leave out. Empty: every route the
// routers register is documented, the content family as the {schema}
// pattern the router itself uses. A route added to the router without a
// document entry fails the test below rather than landing here.
var knownUndocumentedRoutes = map[string]bool{}

func TestRegisteredRoutesAreDocumentedInSpec(t *testing.T) {
	adminRouter, apiRouter := buildTestRouters(t)

	registered := collectRoutes(t, adminRouter)
	for k, v := range collectRoutes(t, apiRouter) {
		registered[k] = v
	}

	doc := buildOpenAPIDoc(nil, nil, openAPIOptions{})
	specPaths := make(map[string]bool, len(doc.Paths)*2)
	for path, methods := range doc.Paths {
		for method := range methods {
			specPaths[strings.ToUpper(method)+" "+path] = true
		}
	}

	var undocumented []string
	for route := range registered {
		// Skip debug, k8s probe, and JWKS routes.
		parts := strings.SplitN(route, " ", 2)
		if len(parts) != 2 {
			continue
		}
		path := parts[1]
		if hasAnyPrefix(path, debugPathPrefixes) ||
			hasAnyPrefix(path, k8sProbePrefixes) {
			continue
		}

		// Known undocumented: skip.
		if knownUndocumentedRoutes[route] {
			continue
		}

		if !specPaths[route] {
			undocumented = append(undocumented, route)
		}
	}

	sort.Strings(undocumented)
	for _, route := range undocumented {
		t.Errorf("route %s is registered but not documented in OpenAPI spec - add to buildOpenAPIDoc or to knownUndocumentedRoutes", route)
	}
}

// An operation a grant opens lists the admin token scheme and names the
// grant. One no grant opens does neither.
func TestOpenAPI_AdminGrantsArePublished(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	doc := buildOpenAPIDoc(nil, []plugin.PluginRoutes{{
		Name: "webhook",
		Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/admin/webhooks", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: "webhooks:read"},
			{Method: http.MethodPost, Pattern: "/api/admin/webhooks", Group: plugin.GroupAdmin, Handler: ok},
		},
	}}, openAPIOptions{AdminTokens: true})
	require.Contains(t, doc.Components.SecuritySchemes, "adminToken")
	scheme := doc.Components.SecuritySchemes["adminToken"].(map[string]any)
	require.Equal(t, "bearer", scheme["scheme"])
	require.Equal(t, "lyat", scheme["bearerFormat"])

	hasTokenScheme := func(op *openAPIOperation) bool {
		for _, s := range op.Security {
			if _, ok := s["adminToken"]; ok {
				return true
			}
		}
		return false
	}
	for _, c := range []struct{ path, method, grant string }{
		// The engine publishes one schema route. The rest belong to the plugin
		// that serves schemas and reach the document through its declaration.
		{"/api/admin/schemas/export", "get", "schemas:read"},
		{"/api/admin/webhooks", "get", "webhooks:read"},
	} {
		op := doc.Paths[c.path][c.method]
		require.NotNil(t, op, "%s %s", c.method, c.path)
		require.Equal(t, c.grant, op.Extensions[xAdminGrant], "%s %s", c.method, c.path)
		require.True(t, hasTokenScheme(op), "%s %s", c.method, c.path)
	}
	for _, c := range []struct{ path, method string }{
		{"/api/admin/webhooks", "post"},
		{"/api/admin/users", "post"},
		{"/api/admin/admin-tokens", "post"},
	} {
		op := doc.Paths[c.path][c.method]
		require.NotNil(t, op, "%s %s", c.method, c.path)
		require.Nil(t, op.Extensions[xAdminGrant], "%s %s", c.method, c.path)
		require.False(t, hasTokenScheme(op), "%s %s", c.method, c.path)
	}
	// The session schemes of an operation that shares its slice with others
	// were not extended in place.
	require.False(t, hasTokenScheme(doc.Paths["/api/admin/users/{id}/roles"]["put"]))
}

// The document describes what this router mounts. A build whose licensing
// implementation keeps no withheld set mounts neither tenant-features route,
// and documenting them anyway sends a client straight at a 404.
func TestOpenAPI_TenantFeaturesAppearOnlyWhenTheRoutesDo(t *testing.T) {
	const path = "/api/admin/tenant-features/{tenant}"
	assert.NotContains(t, buildOpenAPIDoc(nil, nil, openAPIOptions{}).Paths, path)
	assert.Contains(t, buildOpenAPIDoc(nil, nil, openAPIOptions{TenantFeatures: true}).Paths, path)
}

// The document describes what this build serves. Admin tokens need a plugin
// to keep them, so a build with none serves none of those routes, and naming
// them in the document would advertise six routes that answer 404.
func TestOpenAPI_AdminTokenPathsFollowTheStore(t *testing.T) {
	withStore := buildOpenAPIDoc(nil, nil, openAPIOptions{AdminTokens: true})
	require.NotNil(t, withStore.Paths["/api/admin/admin-tokens"], "a build that keeps tokens must document the routes")

	without := buildOpenAPIDoc(nil, nil, openAPIOptions{})
	for _, path := range []string{
		"/api/admin/admin-tokens",
		"/api/admin/admin-tokens/grants",
		"/api/admin/admin-tokens/{id}",
		"/api/admin/admin-tokens/{id}/rotate",
		"/api/admin/admin-tokens/{id}/requests",
	} {
		require.Nil(t, without.Paths[path], path)
	}
}

// Cross-tenant membership is the tenancy plugin's, so the engine's own
// document names none of its routes. They arrive from the plugin's route
// declarations on a build that runs it.
func TestOpenAPI_MembershipPathsAreNotTheEngines(t *testing.T) {
	doc := buildOpenAPIDoc(nil, nil, openAPIOptions{AdminTokens: true, TenantFeatures: true})
	for _, path := range []string{
		"/api/admin/users/{id}/memberships",
		"/api/admin/users/{id}/memberships/{tenant}",
		"/api/admin/tenant-members/{tenant}",
	} {
		require.Nil(t, doc.Paths[path], path)
	}
	// The caller's own list stays: it is part of the auth surface and answers
	// the home tenant whether or not anything supplies memberships.
	require.NotNil(t, doc.Paths["/api/admin/auth/memberships"])
}
