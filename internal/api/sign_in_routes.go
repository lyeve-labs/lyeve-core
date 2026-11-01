package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// signInPrefix is where the admin server's ways in live, the engine's own and
// the plugins'.
const signInPrefix = "/api/admin/auth/"

// signInRoute is one way in that a plugin mounted on the admin server.
type signInRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// signInRoutesHandler answers which public routes the plugins mounted under
// the sign-in prefix, such as a password reset or a sign-in link by email.
// A sign-in page offers each of those only when its route is there. Asking
// each route with a request it refuses was the only other way to learn that,
// and every refusal counted as a failed request.
//
// The list is read from the declarations this router mounted, so it follows
// a license change the way the routes do: the router is built again, and
// this answer with it. A plugin withheld from the caller's tenant refuses its
// routes there, so the answer leaves them out for that tenant. A route with a
// path parameter is a later step reached from a link or a redirect, never a
// choice on the page, so the answer does not name it.
func signInRoutesHandler(pluginRoutes []plugin.PluginRoutes, withholds func(tenant, name string) bool) http.HandlerFunc {
	type mounted struct {
		plugin string
		route  signInRoute
	}
	all := []mounted{}
	for _, pr := range pluginRoutes {
		for _, rd := range pr.Routes {
			if rd.Group == plugin.GroupPublic && strings.HasPrefix(rd.Pattern, signInPrefix) && !strings.Contains(rd.Pattern, "{") {
				all = append(all, mounted{plugin: pr.Name, route: signInRoute{Method: strings.ToUpper(rd.Method), Path: rd.Pattern}})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].route.Path != all[j].route.Path {
			return all[i].route.Path < all[j].route.Path
		}
		return all[i].route.Method < all[j].route.Method
	})
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := core.TenantIDFromCtx(r.Context())
		routes := []signInRoute{}
		for _, m := range all {
			if withholds == nil || !withholds(tenant, m.plugin) {
				routes = append(routes, m.route)
			}
		}
		respond(w, http.StatusOK, map[string]any{"routes": routes})
	}
}
