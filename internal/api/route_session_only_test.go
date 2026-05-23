package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// A route declared session only takes no grant from any declaration: its
// own, another plugin's for the same route, or the engine's own table. Each
// stops the admin router from building, and the error names the plugin.
func TestAdminRouter_RefusesAGrantOnADeclaredSessionOnlyRoute(t *testing.T) {
	t.Parallel()
	ok := servesOK()
	cases := []struct {
		name   string
		routes []plugin.PluginRoutes
		want   string
	}{
		{
			name: "same declaration",
			routes: []plugin.PluginRoutes{{Name: "widgets", Routes: []plugin.RouteDecl{
				{Method: http.MethodPost, Pattern: "/api/admin/widgets/{id}/rotate", Group: plugin.GroupAdmin, Handler: ok, SessionOnly: true, AdminGrant: core.AdminGrantContentWrite},
			}}},
			want: "plugin widgets: route POST /api/admin/widgets/{id}/rotate is session only",
		},
		{
			name: "another plugin's grant on the route",
			routes: []plugin.PluginRoutes{
				{Name: "widgets", Routes: []plugin.RouteDecl{
					{Method: http.MethodPost, Pattern: "/api/admin/widgets/{id}/rotate", Group: plugin.GroupAdmin, Handler: ok, SessionOnly: true},
				}},
				{Name: "gadgets", Routes: []plugin.RouteDecl{
					{Method: http.MethodPost, Pattern: "/api/admin/widgets/{widget}/rotate", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantContentWrite},
				}},
			},
			want: "plugin gadgets: route POST /api/admin/widgets/{widget}/rotate is session only",
		},
		{
			name: "the engine's grant on the route",
			routes: []plugin.PluginRoutes{{Name: "widgets", Routes: []plugin.RouteDecl{
				{Method: http.MethodGet, Pattern: "/api/admin/schemas/export", Group: plugin.GroupAdmin, Handler: ok, SessionOnly: true},
			}}},
			want: "plugin widgets: route GET /api/admin/schemas/export is declared session only, and the engine grants it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)), WithPluginRoutes(tc.routes))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A session-only declaration with no grant anywhere builds, and so does a
// grant on a literal route beside it, which is another route.
func TestAdminRouter_BuildsBesideADeclaredSessionOnlyRoute(t *testing.T) {
	t.Parallel()
	ok := servesOK()
	routes := []plugin.PluginRoutes{{Name: "widgets", Routes: []plugin.RouteDecl{
		{Method: http.MethodPost, Pattern: "/api/admin/widgets/{id}/rotate", Group: plugin.GroupAdmin, Handler: ok, SessionOnly: true},
		{Method: http.MethodPost, Pattern: "/api/admin/widgets/export/rotate", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantContentWrite},
	}}}
	_, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	require.NoError(t, err)
}

// The document offers a route to admin tokens only where the router would
// let one in, so a route declared session only never carries the grant or
// the token scheme, even when a declaration names a grant for it.
func TestOpenAPI_DeclaredSessionOnlyRouteIsNotOfferedToTokens(t *testing.T) {
	ok := servesOK()
	doc := buildOpenAPIDoc(nil, []plugin.PluginRoutes{
		{Name: "widgets", Routes: []plugin.RouteDecl{
			{Method: http.MethodPost, Pattern: "/api/admin/widgets/{id}/rotate", Group: plugin.GroupAdmin, Handler: ok, SessionOnly: true},
			{Method: http.MethodGet, Pattern: "/api/admin/widgets", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantContentRead},
		}},
		{Name: "gadgets", Routes: []plugin.RouteDecl{
			{Method: http.MethodPost, Pattern: "/api/admin/widgets/{id}/rotate", Group: plugin.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantContentWrite},
		}},
	}, openAPIOptions{AdminTokens: true})

	rotate := doc.Paths["/api/admin/widgets/{id}/rotate"]["post"]
	require.NotNil(t, rotate)
	assert.Nil(t, rotate.Extensions[xAdminGrant])
	for _, s := range rotate.Security {
		assert.NotContains(t, s, "adminToken")
	}

	list := doc.Paths["/api/admin/widgets"]["get"]
	require.NotNil(t, list)
	assert.Equal(t, core.AdminGrantContentRead, list.Extensions[xAdminGrant], "a granted route beside it is still offered")
}
