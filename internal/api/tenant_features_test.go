//go:build !mutest

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestTenantGate_RefusesWithheldPluginRoutes(t *testing.T) {
	withholds := withheldFrom(map[string][]string{"acme": {"search"}})

	routes := []plugin.PluginRoutes{
		{Name: "search", Routes: []plugin.RouteDecl{
			{Method: "GET", Pattern: "/api/admin/search/query", Group: plugin.GroupAdmin, Handler: okHandler()},
			{Method: "GET", Pattern: "/api/admin/search/reindex", Group: plugin.GroupSuperAdmin, Handler: okHandler()},
			{Method: "GET", Pattern: "/api/admin/search/public", Group: plugin.GroupPublic, Handler: okHandler()},
			{Method: "GET", Pattern: "/api/admin/search/mine", Group: plugin.GroupAuth, Handler: okHandler()},
		}},
		{Name: "media", Routes: []plugin.RouteDecl{
			{Method: "GET", Pattern: "/api/admin/media", Group: plugin.GroupAdmin, Handler: okHandler()},
		}},
	}
	r := chi.NewRouter()
	mountDeclaredRoutes(r, routes, nil, withholds, "/api/admin", false, 1<<20, nil, nil, nil)

	call := func(path, tenant string, roles ...string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		ctx := context.WithValue(req.Context(), auth.ClaimsKey, makeClaims(uuid.New(), "a@test.com", roles))
		req = req.WithContext(core.WithTenantID(ctx, tenant))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusForbidden, call("/search/query", "acme", "admin"), "withheld plugin")
	assert.Equal(t, http.StatusOK, call("/search/query", "globex", "admin"), "another tenant")
	assert.Equal(t, http.StatusForbidden, call("/search/public", "acme"), "public group")
	assert.Equal(t, http.StatusForbidden, call("/search/mine", "acme", "editor"), "auth group")
	assert.Equal(t, http.StatusOK, call("/media", "acme", "admin"), "a plugin not withheld")
	// The super admin group runs the instance and is never gated.
	assert.Equal(t, http.StatusOK, call("/search/reindex", "acme", "super_admin"), "super admin route")
}
