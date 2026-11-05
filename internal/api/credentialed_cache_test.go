package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// A credential's tenant wins over the host, so a request to one tenant's host
// that carries another tenant's credential is answered from the credential's
// tenant. A public route that caches its answer as public would then let a
// CDN keyed on host and URL store that answer, or that tenant's 404, under
// the host's URL for every visitor. Whatever a route sets, a response to a
// request that carries a credential is one no shared cache may store, and
// one with no Cache-Control at all is not stored anywhere.
func TestAPIRouter_CredentialedPublicRouteIsPrivate(t *testing.T) {
	t.Parallel()

	file := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			// A route that answers without a Cache-Control of its own.
			w.Header().Del("Cache-Control")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=600")
		w.Header().Set("CDN-Cache-Control", "max-age=600")
		w.Header().Set("Cloudflare-CDN-Cache-Control", "max-age=600")
		w.Header().Set("Surrogate-Control", "max-age=600")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	})
	routes := []plugin.PluginRoutes{{
		Name: "files",
		Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/v1/files/{id}", Group: plugin.GroupPublic, Handler: file},
		},
	}}
	cfg := testConfig()
	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil,
		WithLifetime(testLifetime(t)),
		WithPluginRoutes(routes))
	require.NoError(t, err)

	token, err := auth.Sign(cfg.JWTSecrets[0], 3600, uuid.New(), "reader@example.com", []string{"viewer"}, "acme", 1)
	require.NoError(t, err)

	credentials := []struct {
		name string
		set  func(*http.Request)
	}{
		{"bearer token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }},
		{"API key", func(r *http.Request) { r.Header.Set("X-API-Key", "lyeve_test_key") }},
		{"session cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: token})
		}},
	}
	serve := func(path string, set func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "globex.example.com"
		if set != nil {
			set(req)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	for _, c := range credentials {
		t.Run(c.name+" on the 200", func(t *testing.T) {
			rec := serve("/api/v1/files/logo", c.set)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, "private, max-age=300", rec.Header().Get("Cache-Control"))
			for _, h := range []string{"CDN-Cache-Control", "Cloudflare-CDN-Cache-Control", "Surrogate-Control"} {
				assert.Empty(t, rec.Header().Values(h), h)
			}
		})
		t.Run(c.name+" on the 404", func(t *testing.T) {
			rec := serve("/api/v1/files/missing", c.set)
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
		})
	}

	t.Run("no credential keeps the route's own headers", func(t *testing.T) {
		rec := serve("/api/v1/files/logo", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "public, max-age=300, s-maxage=600", rec.Header().Get("Cache-Control"))
		assert.Equal(t, "max-age=600", rec.Header().Get("CDN-Cache-Control"))
	})
}
