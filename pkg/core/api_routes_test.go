package core

import (
	"strings"
	"testing"
)

// prefixRoutes owns every path under one prefix.
type prefixRoutes struct{ prefix string }

func (r prefixRoutes) Owns(path string) bool { return strings.HasPrefix(path, r.prefix) }

type routesHost struct {
	*stubHost
	routes APIRoutes
}

func (h *routesHost) APIRoutes() APIRoutes { return h.routes }

// A plugin reaches the router's answer through its scoped host, and finds
// nil while the runtime has registered none.
func TestScopedHost_APIRoutes_ForwardsTheRegisteredAnswer(t *testing.T) {
	inner := &routesHost{stubHost: &stubHost{}}
	var host Host = NewScopedHost(inner, "flow", CapAll)
	p, ok := host.(APIRoutesProvider)
	if !ok {
		t.Fatal("ScopedHost must satisfy APIRoutesProvider through the Host interface")
	}
	if p.APIRoutes() != nil {
		t.Fatal("APIRoutes() must be nil before the runtime registers one")
	}

	inner.routes = prefixRoutes{prefix: "/api/v1/content"}
	got := p.APIRoutes()
	if got == nil || !got.Owns("/api/v1/content/posts") || got.Owns("/api/v1/orders") {
		t.Fatalf("APIRoutes() = %v, want the registered answer", got)
	}
}

func TestScopedHost_APIRoutes_NilWhenInnerCannotProvide(t *testing.T) {
	sh := NewScopedHost(&stubHost{}, "flow", CapAll)
	if sh.APIRoutes() != nil {
		t.Fatal("APIRoutes() must be nil when the inner host has none")
	}
}

func TestFlowSummary_AdmitsOnlyTheDeclaredSurfaces(t *testing.T) {
	f := FlowSummary{Slug: "orders", Surfaces: []string{FlowSurfaceGraphQL, FlowSurfaceRealtime}}
	if !f.Admits(FlowSurfaceGraphQL) || !f.Admits(FlowSurfaceRealtime) {
		t.Error("a declared surface must be admitted")
	}
	if f.Admits(FlowSurfaceGRPC) || (FlowSummary{}).Admits(FlowSurfaceGraphQL) {
		t.Error("a surface the trigger does not declare must not be admitted")
	}
}
