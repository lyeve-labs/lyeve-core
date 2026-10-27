package api

import (
	"net/http"
	"sort"
	"strings"

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
// this answer with it.
func signInRoutesHandler(pluginRoutes []plugin.PluginRoutes) http.HandlerFunc {
	routes := []signInRoute{}
	for _, pr := range pluginRoutes {
		for _, rd := range pr.Routes {
			if rd.Group == plugin.GroupPublic && strings.HasPrefix(rd.Pattern, signInPrefix) {
				routes = append(routes, signInRoute{Method: strings.ToUpper(rd.Method), Path: rd.Pattern})
			}
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
	body := map[string]any{"routes": routes}
	return func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, body)
	}
}
