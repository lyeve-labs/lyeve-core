package api

import (
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// RouteIndex answers core.APIRoutes from the router a HandlerSource holds
// now. The namespaces are read from the routing tree once per router, so a
// hot reload that swaps the router is picked up on the next question.
type RouteIndex struct {
	src func() http.Handler

	mu         sync.Mutex
	built      http.Handler
	namespaces map[string]bool
	// open is set when a registered pattern opens with a parameter or a
	// wildcard at a namespace position, so every path under its parent is
	// the router's.
	open map[string]bool
}

var _ core.APIRoutes = (*RouteIndex)(nil)

// NewRouteIndex returns an index over the handler src returns at the time
// of each question.
func NewRouteIndex(src func() http.Handler) *RouteIndex {
	return &RouteIndex{src: src}
}

// Owns implements core.APIRoutes.
func (x *RouteIndex) Owns(p string) bool {
	if p == "" || !strings.HasPrefix(p, "/") {
		return true
	}
	clean := path.Clean(p)
	if clean == "/api" || clean == "/api/v1" {
		return true
	}
	h := x.src()
	routes, ok := h.(chi.Routes)
	if !ok {
		// A router the index cannot read owns everything: a path accepted
		// here without a check could be one the engine answers.
		return true
	}
	for _, m := range methodProbeOrder {
		if routes.Match(chi.NewRouteContext(), m, clean) {
			return true
		}
	}
	namespaces, open := x.index(h, routes)
	if ns, ok := namespaceOf(clean); ok {
		if namespaces[ns] {
			return true
		}
		if open[parentOf(ns)] {
			return true
		}
	}
	return false
}

// index returns the namespaces of the handler, reading them again only
// when the handler changed.
func (x *RouteIndex) index(h http.Handler, routes chi.Routes) (map[string]bool, map[string]bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.built == h && x.namespaces != nil {
		return x.namespaces, x.open
	}
	namespaces := map[string]bool{}
	open := map[string]bool{}
	_ = chi.Walk(routes, func(_ string, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		ns, ok := namespaceOf(pattern)
		if !ok {
			return nil
		}
		last := ns[strings.LastIndex(ns, "/")+1:]
		if strings.HasPrefix(last, "{") || last == "*" {
			open[parentOf(ns)] = true
			return nil
		}
		namespaces[ns] = true
		return nil
	})
	x.built, x.namespaces, x.open = h, namespaces, open
	return namespaces, open
}

// namespaceOf is the path cut after its namespace segment: the first
// segment after /api/v1/, else after /api/, else the first segment. A path
// with no segment at that position has no namespace.
func namespaceOf(p string) (string, bool) {
	var base, rest string
	switch {
	case strings.HasPrefix(p, "/api/v1/"):
		base, rest = "/api/v1", p[len("/api/v1/"):]
	case strings.HasPrefix(p, "/api/"):
		base, rest = "/api", p[len("/api/"):]
	default:
		rest = strings.TrimPrefix(p, "/")
	}
	seg, _, _ := strings.Cut(rest, "/")
	if seg == "" {
		return "", false
	}
	return base + "/" + seg, true
}

func parentOf(ns string) string {
	i := strings.LastIndex(ns, "/")
	if i <= 0 {
		return "/"
	}
	return ns[:i]
}

// customRoutesFirst offers a request under a mounted prefix to the custom
// route provider before the prefix's own middleware runs, when no route
// registered under the prefix matches its path for any method. A route the
// engine or a plugin registered always wins, a 405 included, because the
// provider is asked only about a path nothing under the prefix knows. What
// the provider serves passes the outer router's guards and nothing of the
// prefix's chain: no credentials are read and no tenant is resolved, the
// same position the not-found slot gives a path outside the prefix.
func customRoutesFirst(sub chi.Routes, p core.CustomRouteProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if p == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			rel := "/"
			if rctx := chi.RouteContext(req.Context()); rctx != nil && rctx.RoutePath != "" {
				rel = rctx.RoutePath
			}
			if sub.Match(chi.NewRouteContext(), req.Method, rel) || routedForAnyMethod(sub, rel) {
				next.ServeHTTP(w, req)
				return
			}
			if h := p.CustomRoute(req); h != nil {
				h.ServeHTTP(w, req)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

func routedForAnyMethod(r chi.Routes, p string) bool {
	for _, m := range methodProbeOrder {
		if r.Match(chi.NewRouteContext(), m, p) {
			return true
		}
	}
	return false
}
