package core

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSessionOnlyAdminRoute(t *testing.T) {
	cases := []struct {
		method, pattern string
		want            bool
	}{
		{http.MethodGet, "/api/admin/users", true},
		{http.MethodPut, "/api/admin/users/{id}/roles", true},
		{http.MethodGet, "/api/admin/admin-tokens/{id}/requests", true},
		{http.MethodPost, "/api/admin/auth/device/{user_code}/approve", true},
		{http.MethodPut, "/api/admin/config", true},
		{http.MethodPut, "/api/admin/plugins/{plugin}/config", true},
		{http.MethodPost, "/api/admin/plugins/{name}/config/reset", true},
		{http.MethodPost, "/api/admin/schemas/import", true},
		{http.MethodPost, "/api/admin/gdpr/erase", true},
		{http.MethodGet, "/api/admin/debug/pprof/heap", true},
		{http.MethodPost, "/api/admin/debug/gc-config", true},

		// Reads and routes the list leaves open.
		{http.MethodGet, "/api/admin/debug/gc-config", false},
		{http.MethodGet, "/api/admin/tenants/{id}", false},
		{http.MethodGet, "/api/admin/schemas", false},
		{http.MethodGet, "/api/admin/schemas/{name}", false},
		{http.MethodGet, "/api/admin/usersettings", false},
		{http.MethodGet, "/api/admin/webhooks", false},

		// Routes the engine does not serve are session only by their own
		// declarations, which this call is not given.
		{http.MethodPost, "/api/admin/api-keys", false},
		{http.MethodPost, "/api/admin/tenants/{id}/archive", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsSessionOnlyAdminRoute(tc.method, tc.pattern), "%s %s", tc.method, tc.pattern)
	}
}

func TestValidateAdminGrants(t *testing.T) {
	require.NoError(t, ValidateAdminGrants([]RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/webhooks", AdminGrant: AdminGrantWebhooksRead},
		{Method: http.MethodGet, Pattern: "/api/admin/users"},
	}))

	err := ValidateAdminGrants([]RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/webhooks", AdminGrant: "webhooks:admin"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the catalog")

	err = ValidateAdminGrants([]RouteDecl{{Method: http.MethodPost, Pattern: "/api/admin/admin-tokens", AdminGrant: AdminGrantContentWrite}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session only")

	err = ValidateAdminGrants([]RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/webhooks/deliveries", Group: GroupSuperAdmin, AdminGrant: AdminGrantWebhooksRead}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "super admin only")
}

func TestAdminGrants_CatalogIsClosedAndDescribed(t *testing.T) {
	grants := AdminGrants()
	require.Len(t, grants, 13)
	seen := map[string]bool{}
	for _, g := range grants {
		assert.True(t, IsAdminGrant(g.Name), g.Name)
		assert.NotEmpty(t, g.Description, g.Name)
		assert.False(t, seen[g.Name], "duplicate %s", g.Name)
		seen[g.Name] = true
	}
	assert.False(t, IsAdminGrant("users:write"))
	assert.False(t, IsAdminGrant(""))

	// The copy is the caller's.
	grants[0].Name = "changed"
	assert.Equal(t, AdminGrantContentRead, AdminGrants()[0].Name)
}

// A declaration marks its own route session only: the same method and the
// same pattern whatever its parameters are called. A literal route beside it
// is another route, and the engine's patterns still apply.
func TestIsSessionOnlyAdminRoute_ReadsDeclarations(t *testing.T) {
	declared := []RouteDecl{
		{Method: http.MethodPost, Pattern: "/api/admin/widgets/{id}/rotate", SessionOnly: true},
		{Method: http.MethodGet, Pattern: "/api/admin/widgets/{id}"},
	}
	cases := []struct {
		method, pattern string
		want            bool
	}{
		{http.MethodPost, "/api/admin/widgets/{id}/rotate", true},
		{http.MethodPost, "/api/admin/widgets/{widget}/rotate", true},
		{"post", "/api/admin/widgets/{id}/rotate", true},
		{http.MethodPut, "/api/admin/widgets/{id}/rotate", false},
		{http.MethodPost, "/api/admin/widgets/export/rotate", false},
		{http.MethodPost, "/api/admin/widgets/{id}/rotate/now", false},
		{http.MethodGet, "/api/admin/widgets/{id}", false},
		{http.MethodPost, "/api/admin/admin-tokens", true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsSessionOnlyAdminRoute(tc.method, tc.pattern, declared...), "%s %s", tc.method, tc.pattern)
	}
	assert.False(t, IsSessionOnlyAdminRoute(http.MethodPost, "/api/admin/widgets/{id}/rotate"),
		"without the declaration the route is not session only")
}

// A grant on a route marked session only is refused, whether the mark is in
// the declaration that carries the grant or in another one.
func TestValidateAdminGrants_RefusesAGrantOnADeclaredSessionOnlyRoute(t *testing.T) {
	err := ValidateAdminGrants([]RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/widgets", Group: GroupAdmin, AdminGrant: AdminGrantContentRead, SessionOnly: true}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session only")

	elsewhere := RouteDecl{Method: http.MethodGet, Pattern: "/api/admin/widgets/{id}", Group: GroupAdmin, SessionOnly: true}
	err = ValidateAdminGrants([]RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/widgets/{slug}", Group: GroupAdmin, AdminGrant: AdminGrantContentRead}}, elsewhere)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session only")

	require.NoError(t, ValidateAdminGrants([]RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/widgets/export", Group: GroupAdmin, AdminGrant: AdminGrantContentRead}}, elsewhere),
		"a literal route beside a session-only one is another route")
}
