package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// A route that carries a file rather than a document declares the body it
// takes, and both guards have to admit it: the group's JSON limit, and the
// global one mounted ahead of routing. Declaring to the first alone would
// leave the second refusing the route below the size it publishes.
func TestPluginRoutes_DeclaredBodyLimitPassesBothGuards(t *testing.T) {
	t.Parallel()

	const groupJSONLimit int64 = 1024
	const globalLimit int64 = 2048
	const declared int64 = 64 << 10

	routes := []plugin.PluginRoutes{{
		Name: "bulk-import",
		Routes: []plugin.RouteDecl{
			{
				Method:       "POST",
				Pattern:      "/api/admin/imports",
				Group:        plugin.GroupPublic,
				MaxBodyBytes: declared,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			},
			{
				Method:  "POST",
				Pattern: "/api/admin/imports/cancel",
				Group:   plugin.GroupPublic,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			},
		},
	}}

	// Mirrors production: the global guard sits on the root router, ahead of
	// routing, and the plugin routes mount under the prefix.
	r := chi.NewRouter()
	r.Use(apimw.MaxBodySizeFor(globalLimit, pluginBodyOverrides(routes)))
	r.Route("/api/admin", func(sub chi.Router) {
		mountPluginRoutes(sub, routes, "/api/admin", false, groupJSONLimit, nil, nil, nil)
	})

	cases := []struct {
		name string
		path string
		size int
		want int
	}{
		{"declaring route takes a body over both limits", "/api/admin/imports", 8192, http.StatusOK},
		{"declaring route still refuses past what it declared", "/api/admin/imports", int(declared) + 1, http.StatusRequestEntityTooLarge},
		{"a route beside it keeps the group limit", "/api/admin/imports/cancel", 4096, http.StatusRequestEntityTooLarge},
		{"a route beside it still takes a document", "/api/admin/imports/cancel", 512, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("x"), tc.size)
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s with %d bytes: got %d, want %d", tc.path, tc.size, rec.Code, tc.want)
			}
		})
	}
}

// The global guard matches literal paths, so a declaration on a pattern
// holding a path parameter cannot be honored. It is dropped rather than
// applied to the group guard alone, which would leave the route passing one
// limit and refused by the other.
func TestPluginBodyOverrides_IgnoresParameterizedPatterns(t *testing.T) {
	t.Parallel()

	routes := []plugin.PluginRoutes{{
		Name: "test",
		Routes: []plugin.RouteDecl{
			{Method: "POST", Pattern: "/api/admin/imports/{id}/rows", MaxBodyBytes: 1 << 20},
			{Method: "POST", Pattern: "/api/admin/imports", MaxBodyBytes: 1 << 20},
			{Method: "GET", Pattern: "/api/admin/imports"},
		},
	}}

	got := pluginBodyOverrides(routes)
	want := map[string]int64{"POST /api/admin/imports": 1 << 20}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q: got %d, want %d", k, got[k], v)
		}
	}
}
