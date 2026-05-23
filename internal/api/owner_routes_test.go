package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/metrics"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// labelEcho answers 200 and names the plugin label the request carried, so a
// test can see whether a route ran under one.
func labelEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label, _ := r.Context().Value(metrics.PluginCtxKey).(string)
		w.Header().Set("X-Plugin-Label", label)
		w.WriteHeader(http.StatusOK)
	})
}

// withheldFrom answers the route guard the way a licensing implementation's
// manager does, from what each tenant is refused.
func withheldFrom(refused map[string][]string) func(tenant, name string) bool {
	return func(tenant, name string) bool {
		for _, n := range refused[tenant] {
			if n == name {
				return true
			}
		}
		return false
	}
}

// An owner's routes mount in the group stack their declarations name, beside
// the plugins' routes. What a tenant can be denied is a plugin, so the tenant
// gate never refuses an owner's route even when the owner's name is on the
// withheld list, and the route runs under no plugin label.
func TestOwnerRoutes_MountBesideThePluginsWithoutTheTenantGate(t *testing.T) {
	withholds := withheldFrom(map[string][]string{"acme": {"search", "keeper"}})

	plugins := []plugin.PluginRoutes{{Name: "search", Routes: []plugin.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/search/query", Group: plugin.GroupAdmin, Handler: labelEcho()},
	}}}
	owners := []plugin.PluginRoutes{{Name: "keeper", Routes: []plugin.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/keeper/state", Group: plugin.GroupAdmin, Handler: labelEcho()},
		{Method: http.MethodPost, Pattern: "/api/admin/keeper/rotate", Group: plugin.GroupSuperAdmin, Handler: labelEcho()},
		{Method: http.MethodGet, Pattern: "/api/admin/keeper/ping", Group: plugin.GroupPublic, Handler: labelEcho()},
	}}}
	r := chi.NewRouter()
	mountDeclaredRoutes(r, plugins, owners, withholds, "/api/admin", false, 1<<20, nil, nil, nil)

	call := func(method, path string, roles ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		ctx := req.Context()
		if roles != nil {
			ctx = context.WithValue(ctx, auth.ClaimsKey, makeClaims(uuid.New(), "a@test.com", roles))
		}
		req = req.WithContext(core.WithTenantID(ctx, "acme"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	assert.Equal(t, http.StatusForbidden, call(http.MethodGet, "/search/query", "admin").Code, "a withheld plugin is refused")

	state := call(http.MethodGet, "/keeper/state", "admin")
	assert.Equal(t, http.StatusOK, state.Code, "an owner's route is not a plugin a tenant can be denied")
	assert.Empty(t, state.Header().Get("X-Plugin-Label"), "an owner's route runs under no plugin label")

	assert.Equal(t, http.StatusUnauthorized, call(http.MethodGet, "/keeper/state").Code, "the admin group still asks for a credential")
	assert.Equal(t, http.StatusForbidden, call(http.MethodPost, "/keeper/rotate", "admin").Code, "the super admin group still asks for the role")
	assert.Equal(t, http.StatusOK, call(http.MethodPost, "/keeper/rotate", "super_admin").Code)
	assert.Equal(t, http.StatusOK, call(http.MethodGet, "/keeper/ping").Code, "a public route needs no credential")
}

func keeperRoutes() []core.RouteDecl {
	return []core.RouteDecl{
		{Method: http.MethodPost, Pattern: "/api/admin/keeper/renew", Group: core.GroupSuperAdmin, Handler: labelEcho(),
			Sensitive: true, SessionOnly: true,
			Doc: &core.RouteDoc{Tag: "Admin / Keeper", Summary: "Renew the key", Responses: map[int]string{http.StatusOK: "Renewed"}}},
		{Method: http.MethodGet, Pattern: "/api/admin/keeper/status", Group: core.GroupAdmin, Handler: labelEcho(), AdminGrant: core.AdminGrantLogsRead},
		{Method: http.MethodPost, Pattern: "/api/admin/keeper/ping", Group: core.GroupPublic, Handler: labelEcho(),
			RateLimit: &core.RouteRateLimit{Rate: 1, Burst: 2}, MaxBodyBytes: 4 << 20},
	}
}

// An owner's declarations are read for every policy a plugin's are: the
// capture keeps its sensitive bodies out, its public limit joins the table,
// its body ceiling reaches the outer guard, its grants reach the registry,
// and the document describes its routes without naming a license feature.
func TestOwnerRoutes_AreReadForTheirDeclaredPolicy(t *testing.T) {
	sink := &memorySink{}
	opts := []RouterOption{WithOwnerRoutes("keeper", keeperRoutes()), WithRequestCapture(sink)}
	h, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), append(opts, WithLifetime(testLifetime(t)))...)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/keeper/renew", strings.NewReader(`{"key":"secret"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)
	assert.False(t, sink.bodiesStored(t, "/api/admin/keeper/renew"), "a sensitive owner route keeps its bodies out of the capture")

	o := applyRouterOptions(opts)
	table, _ := publicRateLimitTable(o)
	assert.Equal(t, apimw.PublicRateLimitConfig{Rate: 1, Burst: 2}, table["POST:/api/admin/keeper/ping"])
	assert.Equal(t, int64(4<<20), pluginBodyOverrides(o.declaredRouteSets())[apimw.BodyLimitKey(http.MethodPost, "/api/admin/keeper/ping")])

	reg, err := buildAdminGrantRegistry(nil, o.ownerRoutes)
	require.NoError(t, err)
	assert.Equal(t, core.AdminGrantLogsRead, reg.grantFor(http.MethodGet, "/api/admin/keeper/status"))
	assert.True(t, reg.isPublic(http.MethodPost, "/api/admin/keeper/ping"))

	doc := buildOpenAPIDoc(nil, nil, openAPIOptions{AdminTokens: true, OwnerRoutes: o.ownerRoutes})
	renew := doc.Paths["/api/admin/keeper/renew"]["post"]
	require.NotNil(t, renew)
	assert.Equal(t, []string{"Admin / Keeper"}, renew.Tags)
	assert.Equal(t, []string{"super_admin"}, renew.Extensions[xRoles])
	assert.Nil(t, renew.Extensions[xFeature], "an owner that is not a plugin is no license feature")
	status := doc.Paths["/api/admin/keeper/status"]["get"]
	require.NotNil(t, status)
	assert.Equal(t, []string{"keeper"}, status.Tags, "a route with no Doc takes the owner's name as its tag")
	assert.Equal(t, core.AdminGrantLogsRead, status.Extensions[xAdminGrant])
}

// A grant on an owner's session-only route stops the admin router from
// building, and the error names the owner as itself, not as a plugin.
func TestOwnerRoutes_GrantOnASessionOnlyRouteStopsTheBuild(t *testing.T) {
	routes := keeperRoutes()
	routes[0].AdminGrant = core.AdminGrantLogsRead
	_, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)), WithOwnerRoutes("keeper", routes))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin router: keeper: route POST /api/admin/keeper/renew is session only")
}

// The API and stateless routers mount an owner's routes under their own
// prefix as well.
func TestOwnerRoutes_MountOnTheAPIAndStatelessRouters(t *testing.T) {
	routes := []core.RouteDecl{{Method: http.MethodGet, Pattern: "/api/v1/keeper/ping", Group: core.GroupPublic, Handler: labelEcho()}}

	apiRouter, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)), WithOwnerRoutes("keeper", routes))
	require.NoError(t, err)
	stateless := NewStatelessRouter(testConfig(), NewStatelessMode(nil), WithLifetime(testLifetime(t)), WithOwnerRoutes("keeper", routes))

	for name, h := range map[string]http.Handler{"api": apiRouter, "stateless": stateless} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/keeper/ping", nil))
		assert.Equal(t, http.StatusOK, rec.Code, name)
	}
}
