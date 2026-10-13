package api

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// headerFieldNameRe is the token grammar of an HTTP field name (RFC 9110,
// section 5.1).
var headerFieldNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// withDeclaredHeaders returns a configured CORS header list with the names
// declared routes add to it.
//
// The configured list is kept as the operator wrote it, and a declared name it
// already holds, in any case, is not repeated. A declared name that is not a
// field name is logged and left out, because a browser that cannot parse the
// list honors none of it. So is the wildcard, which on a request without
// credentials would open every header to every allowed origin.
func withDeclaredHeaders(configured, field string, sets []plugin.PluginRoutes, declared func(plugin.RouteDecl) []string) string {
	seen := map[string]bool{}
	for _, name := range strings.Split(configured, ",") {
		seen[strings.ToLower(strings.TrimSpace(name))] = true
	}
	out := configured
	for _, pr := range sets {
		for _, rd := range pr.Routes {
			for _, name := range declared(rd) {
				name = strings.TrimSpace(name)
				if name == "*" || !headerFieldNameRe.MatchString(name) {
					slog.Warn("route declares a CORS header that is not a header field name: the declaration is ignored",
						"owner", pr.Name, "method", rd.Method, "pattern", rd.Pattern, "field", field, "header", name)
					continue
				}
				key := strings.ToLower(name)
				if seen[key] {
					continue
				}
				seen[key] = true
				if strings.TrimSpace(out) == "" {
					out = name
				} else {
					out += ", " + name
				}
			}
		}
	}
	return out
}

// exposedHeaders is the Access-Control-Expose-Headers list a router sends:
// CORS_EXPOSE_HEADERS, and every header a declared route exposes.
func exposedHeaders(configured string, sets []plugin.PluginRoutes) string {
	return withDeclaredHeaders(configured, "ExposeHeaders", sets,
		func(rd plugin.RouteDecl) []string { return rd.ExposeHeaders })
}
