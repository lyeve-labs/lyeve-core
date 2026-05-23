package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// keyScopeRoute is one route an API key can call, with the scope it needs.
type keyScopeRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Resource is the scope's resource without its qualifier: "content".
	Resource string `json:"resource"`
	// Name is what qualifies the resource on this route. A path parameter
	// such as "{schema}" means a key may be limited to one value of it, and
	// a literal such as "feed" names this endpoint. Empty means the route's
	// scope takes no qualifier.
	Name   string `json:"name,omitempty"`
	Action string `json:"action"`
	// Scope is the narrowest scope that reaches the route.
	Scope string `json:"scope"`
	// Owner is the plugin that serves the route, or "engine".
	Owner string `json:"owner"`
	// Declared is true when the route names its own scope rather than the
	// one its method and path imply. A declared scope takes no qualifier.
	Declared bool `json:"declared,omitempty"`
}

// kernelKeyRoutes are the engine's own routes under /api/v1 that an API key
// reaches through the scope gate. TestKeyScopeCatalog_KernelRoutesAreServed
// holds this list to the router.
var kernelKeyRoutes = []struct{ method, path string }{
	{http.MethodGet, "/api/v1/schemas"},
	{http.MethodGet, "/api/v1/schemas/{name}"},
	{http.MethodGet, "/api/v1/content/{schema}"},
	{http.MethodGet, "/api/v1/content/{schema}/cursor"},
	{http.MethodGet, "/api/v1/content/{schema}/stream"},
	{http.MethodGet, "/api/v1/content/{schema}/{id}"},
	{http.MethodGet, "/api/v1/content/{schema}/{id}/relations/{field}"},
	{http.MethodGet, "/api/v1/content/{schema}/{id}/revisions"},
	{http.MethodPost, "/api/v1/content/{schema}"},
	{http.MethodPost, "/api/v1/content/{schema}/bulk"},
	{http.MethodPut, "/api/v1/content/{schema}/{id}"},
	{http.MethodDelete, "/api/v1/content/{schema}/{id}"},
	{http.MethodPut, "/api/v1/content/{schema}/{id}/relations/{field}"},
	{http.MethodPut, "/api/v1/content/{schema}/{id}/publish"},
	{http.MethodPut, "/api/v1/content/{schema}/{id}/unpublish"},
	{http.MethodPut, "/api/v1/content/{schema}/{id}/revisions/{rev_id}/restore"},
}

// keyScopeCatalog lists every Content API route an API key can be scoped
// to: the engine's content and schema routes, and every route a plugin or
// another owner declares in the authenticated group under /api/v1. Public
// routes take no key and the admin groups refuse one. The admin API is for
// sessions and admin tokens, so its authenticated routes are not offered
// either. A declaration the engine's own route shadows is listed once, as
// the engine's, because the engine's handler is the one that serves it.
func keyScopeCatalog(pluginRoutes, ownerRoutes []plugin.PluginRoutes) []keyScopeRoute {
	out := make([]keyScopeRoute, 0, len(kernelKeyRoutes))
	seen := make(map[string]bool, len(kernelKeyRoutes))
	for _, kr := range kernelKeyRoutes {
		out = append(out, pathScopeRoute(kr.method, kr.path, "engine"))
		seen[kr.method+" "+kr.path] = true
	}
	add := func(sets []plugin.PluginRoutes) {
		for _, pr := range sets {
			for _, rd := range pr.Routes {
				switch rd.Group {
				case plugin.GroupPublic, plugin.GroupAdmin, plugin.GroupSuperAdmin:
					continue
				}
				if !strings.HasPrefix(rd.Pattern, "/api/v1/") || seen[rd.Method+" "+rd.Pattern] {
					continue
				}
				seen[rd.Method+" "+rd.Pattern] = true
				if rd.Scope == "" {
					out = append(out, pathScopeRoute(rd.Method, rd.Pattern, pr.Name))
					continue
				}
				pair, ok := core.ParseScope(rd.Scope)
				if !ok {
					// Only the full wildcard reaches a route whose scope
					// cannot be read, so it is listed under that.
					pair = core.ScopePair{Resource: "*", Action: "*"}
				}
				out = append(out, keyScopeRoute{
					Method: rd.Method, Path: rd.Pattern, Resource: pair.Resource,
					Action: pair.Action, Scope: pair.Resource + ":" + pair.Action,
					Owner: pr.Name, Declared: true,
				})
			}
		}
	}
	add(pluginRoutes)
	add(ownerRoutes)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Resource != out[j].Resource {
			return out[i].Resource < out[j].Resource
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// pathScopeRoute describes a route whose scope its method and path imply.
func pathScopeRoute(method, pattern, owner string) keyScopeRoute {
	pair := core.BuildScopeRoute(method, pattern)
	resource, name, _ := strings.Cut(pair.Resource, ".")
	return keyScopeRoute{
		Method: method, Path: pattern, Resource: resource, Name: name,
		Action: pair.Action, Scope: pair.Resource + ":" + pair.Action, Owner: owner,
	}
}

// keyScopesHandler answers the scope catalog, so the console builds its key
// form from the routes this binary serves rather than from a list of its own.
func keyScopesHandler(pluginRoutes, ownerRoutes []plugin.PluginRoutes) http.HandlerFunc {
	routes := keyScopeCatalog(pluginRoutes, ownerRoutes)
	return func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"actions": []string{core.ActionRead, core.ActionCreate, core.ActionUpdate, core.ActionDelete},
			"routes":  routes,
		})
	}
}
