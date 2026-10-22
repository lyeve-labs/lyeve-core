package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/panicreport"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// panicRoutes declares one public route per router prefix whose handler
// panics, the way a plugin handler with a defect does.
func panicRoutes() []plugin.PluginRoutes {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler boom")
	})
	return []plugin.PluginRoutes{{
		Name: "probe",
		Routes: []core.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/v1/probe/boom/{token}", Handler: boom, Group: plugin.GroupPublic},
			{Method: http.MethodGet, Pattern: "/api/admin/probe/boom/{token}", Handler: boom, Group: plugin.GroupPublic},
		},
	}}
}

// Every router recovers a handler's panic and answers 500, and the panic is
// also handed to the panic reporter with the tenant the request resolved to,
// so it reaches error tracking instead of stderr alone. The report names the
// matched pattern and carries none of the path's values, the query or the
// credentials the request sent.
func TestRouters_ReportRecoveredPanics(t *testing.T) {
	lifetime := WithLifetime(testLifetime(t))
	routes := WithPluginRoutes(panicRoutes())

	apiRouter, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, lifetime, routes)
	require.NoError(t, err)
	adminRouter, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), lifetime, routes)
	require.NoError(t, err)
	statelessRouter := NewStatelessRouter(testConfig(), NewStatelessMode(nil), lifetime, routes)

	cases := []struct {
		name   string
		router http.Handler
		path   string
	}{
		{"api", apiRouter, "/api/v1/probe/boom/{token}"},
		{"admin", adminRouter, "/api/admin/probe/boom/{token}"},
		{"stateless", statelessRouter, "/api/v1/probe/boom/{token}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []panicreport.Panic
			panicreport.Set(func(p panicreport.Panic) { got = append(got, p) })
			t.Cleanup(func() { panicreport.Set(nil) })

			path := strings.Replace(tc.path, "{token}", "path-secret", 1) + "?key=query-secret"
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-Tenant-ID", core.DefaultTenantSlug)
			req.Header.Set("Authorization", "Bearer header-secret")
			rec := httptest.NewRecorder()
			tc.router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusInternalServerError, rec.Code, "the recoverer still answers the panic")
			require.Len(t, got, 1, "the recovered panic was never reported")
			assert.Equal(t, core.DefaultTenantSlug, got[0].TenantID)
			assert.Equal(t, http.MethodGet, got[0].Method)
			assert.Equal(t, tc.path, got[0].Route, "the pattern, not the path")
			assert.Empty(t, got[0].Goroutine)
			assert.Equal(t, "handler boom", got[0].Value)
			assert.Contains(t, string(got[0].Stack), "panicRoutes.func1", "the stack reaches the panicking handler")
			assert.NotContains(t, fmt.Sprintf("%+v", got[0]), "secret", "nothing the request sent reaches the report")
		})
	}
}

// A client that goes away mid-response is aborted with http.ErrAbortHandler,
// which the server treats as a normal end. It is not a defect to report.
func TestRouters_AbortedHandlerIsNotReported(t *testing.T) {
	abort := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	r, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)),
		WithPluginRoutes([]plugin.PluginRoutes{{Name: "probe", Routes: []core.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/v1/probe/abort", Handler: abort, Group: plugin.GroupPublic},
		}}}))
	require.NoError(t, err)

	reported := false
	panicreport.Set(func(panicreport.Panic) { reported = true })
	t.Cleanup(func() { panicreport.Set(nil) })

	assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/probe/abort", nil))
	})
	assert.False(t, reported)
}
