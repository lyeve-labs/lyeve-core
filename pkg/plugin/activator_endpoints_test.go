package plugin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// endpointPlugin serves one path under its own name and lists one endpoint,
// or fails to list when err is set.
type endpointPlugin struct {
	plainPlugin
	path string
	err  error
}

func (p *endpointPlugin) CustomRoute(r *http.Request) http.Handler {
	if r.URL.Path != p.path {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Served-By", p.name)
	})
}

func (p *endpointPlugin) DocumentedEndpoints(context.Context) ([]core.DocumentedEndpoint, error) {
	if p.err != nil {
		return nil, p.err
	}
	return []core.DocumentedEndpoint{{Method: "POST", Path: p.path, Tag: p.name}}, nil
}

func servedBy(t *testing.T, a *Activator, path string) string {
	t.Helper()
	h := a.CustomRoute(httptest.NewRequest(http.MethodPost, path, nil))
	if h == nil {
		return ""
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec.Header().Get("X-Served-By")
}

// Two plugins claiming one path resolve by name, the same way on every
// request, and a plugin that implements neither role is skipped.
func TestActivator_CustomRouteAsksRunningPluginsInNameOrder(t *testing.T) {
	a := activatorWith(
		&endpointPlugin{plainPlugin: plainPlugin{name: "zeta"}, path: "/shared"},
		&endpointPlugin{plainPlugin: plainPlugin{name: "alpha"}, path: "/shared"},
		&endpointPlugin{plainPlugin: plainPlugin{name: "gamma"}, path: "/hooks/x"},
		&plainPlugin{name: "beta"},
	)
	a.active["nil-slot"] = nil
	a.active["never-started"] = &activatedPlugin{}

	for range 5 {
		assert.Equal(t, "alpha", servedBy(t, a, "/shared"))
	}
	assert.Equal(t, "gamma", servedBy(t, a, "/hooks/x"))
	assert.Empty(t, servedBy(t, a, "/nothing"))
}

func TestActivator_DocumentedEndpointsLeavesAFailingPluginOut(t *testing.T) {
	a := activatorWith(
		&endpointPlugin{plainPlugin: plainPlugin{name: "gamma"}, path: "/hooks/x"},
		&endpointPlugin{plainPlugin: plainPlugin{name: "broken"}, path: "/b", err: errors.New("store down")},
	)
	eps, err := a.DocumentedEndpoints(context.Background())
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, "/hooks/x", eps[0].Path)
}
