package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// domainPlugin resolves a tenant from the request the way a domain-routing
// plugin does, and puts it on the context for TenantHeader to find.
type domainPlugin struct {
	*fakePlugin
	tenant string
	extra  []func(http.Handler) http.Handler
}

func (p *domainPlugin) PreAuthMiddleware() []func(http.Handler) http.Handler {
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(core.WithTenantID(r.Context(), p.tenant)))
		})
	}
	return append([]func(http.Handler) http.Handler{mw}, p.extra...)
}

// tagMiddleware appends a marker to order so a test can read the order the
// activator asked for the middleware back.
func tagMiddleware(order *[]string, tag string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*order = append(*order, tag)
			next.ServeHTTP(w, r)
		})
	}
}

func startWith(t *testing.T, features []string) *Activator {
	t.Helper()
	a := NewActivator(testHost{}, nil)
	a.Resolve(grants(features...), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return a
}

// run drives a chain of middleware once and returns the tenant the innermost
// handler saw.
func run(chain []func(http.Handler) http.Handler) string {
	var seen string
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = core.TenantIDFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/pages", nil))
	return seen
}

// A running plugin that implements core.PreAuthMiddlewareProvider has its
// middleware collected, so a plugin able to name the tenant for an anonymous
// request has a place to say so.
func TestPreAuthMiddleware_RunningProviderIsCollected(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)

	p := &domainPlugin{fakePlugin: newFakePlugin("gamma"), tenant: "acme"}
	RegisterPlugin("gamma", func() core.Plugin { return p })

	a := startWith(t, []string{"gamma"})

	chain := a.PreAuthMiddleware()
	if len(chain) != 1 {
		t.Fatalf("collected %d middleware; want 1", len(chain))
	}
	if got := run(chain); got != "acme" {
		t.Errorf("handler saw tenant %q; want %q", got, "acme")
	}
}

// A plugin that does not implement the capability is the ordinary case here,
// not a wiring defect: the engine asks every running plugin rather than one
// it expects to answer.
func TestPreAuthMiddleware_PluginWithoutCapabilityContributesNothing(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)

	RegisterPlugin("beta", func() core.Plugin { return newFakePlugin("beta") })

	a := startWith(t, []string{"beta"})

	if chain := a.PreAuthMiddleware(); chain != nil {
		t.Errorf("collected %d middleware from a plugin that implements nothing; want none", len(chain))
	}
}

// An install with no such plugin must mount nothing extra, so the accessor
// has to return nil rather than an empty slice.
func TestPreAuthMiddleware_NoPluginsIsNil(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)

	a := startWith(t, nil)

	if chain := a.PreAuthMiddleware(); chain != nil {
		t.Errorf("collected %d middleware with no plugin active; want nil", len(chain))
	}
}

// Two providers must mount in the same order on every boot. The map they are
// held in iterates randomly, so without a sort this is one replica answering
// differently from the rest.
func TestPreAuthMiddleware_ProvidersRunInNameOrder(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)

	var order []string
	zed := &domainPlugin{fakePlugin: newFakePlugin("zed"), tenant: "zed",
		extra: []func(http.Handler) http.Handler{tagMiddleware(&order, "zed")}}
	alpha := &domainPlugin{fakePlugin: newFakePlugin("alpha"), tenant: "alpha",
		extra: []func(http.Handler) http.Handler{tagMiddleware(&order, "alpha")}}
	RegisterPlugin("zed", func() core.Plugin { return zed })
	RegisterPlugin("alpha", func() core.Plugin { return alpha })

	a := startWith(t, []string{"zed", "alpha"})

	chain := a.PreAuthMiddleware()
	if len(chain) != 4 {
		t.Fatalf("collected %d middleware; want 4", len(chain))
	}
	// The later provider wins the tenant, which is what name order decides.
	if got := run(chain); got != "zed" {
		t.Errorf("handler saw tenant %q; want %q", got, "zed")
	}
	if len(order) != 2 || order[0] != "alpha" || order[1] != "zed" {
		t.Errorf("middleware ran in order %v; want [alpha zed]", order)
	}
}

// A stopped plugin keeps its entry in the active map. Asking a torn-down
// plugin for middleware would mount a handler over a closed store.
func TestPreAuthMiddleware_StoppedPluginIsSkipped(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)

	p := &domainPlugin{fakePlugin: newFakePlugin("gamma"), tenant: "acme"}
	RegisterPlugin("gamma", func() core.Plugin { return p })

	a := startWith(t, []string{"gamma"})
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if chain := a.PreAuthMiddleware(); chain != nil {
		t.Errorf("collected %d middleware from a stopped plugin; want none", len(chain))
	}
}
