package plugintest

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// knownMethods are the methods the engine's router mounts. A declaration
// naming anything else is never reached, and the route silently does not
// exist.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true,
}

var knownGroups = map[core.RouteGroup]bool{
	core.GroupPublic: true, core.GroupAuth: true,
	core.GroupAdmin: true, core.GroupSuperAdmin: true,
}

// AssertRouteContract checks every invariant a plugin's route declarations must
// hold for the engine to mount them as the plugin intends.
//
// It reads the declarations rather than serving them. What a handler answers
// depends on middleware the engine mounts ahead of it, so a handler exercised
// on its own proves nothing about the route. What the declaration says is the
// whole of the plugin's side of the contract, and every rule below is one
// RouteDecl's own documentation states.
func AssertRouteContract(t T, pluginName string, routes []core.RouteDecl) {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper()
	}
	if len(routes) == 0 {
		t.Errorf("%s declares no routes; a plugin that serves none should not assert a route contract", pluginName)
		return
	}

	seen := map[string]string{}
	for _, r := range routes {
		where := fmt.Sprintf("%s: %s %s", pluginName, r.Method, r.Pattern)

		if r.Handler == nil {
			t.Errorf("%s has a nil handler, which panics on the first request", where)
		}
		if !knownMethods[r.Method] {
			t.Errorf("%s names a method the router does not mount, so the route does not exist", where)
		}
		if !knownGroups[r.Group] {
			t.Errorf("%s declares group %q, which is not one the engine gates on", where, r.Group)
		}
		if msg := patternFault(r.Pattern); msg != "" {
			t.Errorf("%s: %s", where, msg)
		}

		// The outermost body guard runs before routing and matches paths, so a
		// limit on a pattern holding a parameter is logged and ignored. A
		// plugin declaring one is publishing a size it cannot receive.
		if r.MaxBodyBytes > 0 && strings.Contains(r.Pattern, "{") {
			t.Errorf("%s declares MaxBodyBytes on a pattern with a parameter; the guard matches paths, so the limit is ignored", where)
		}

		// SelfScoping exempts a route from the rule that a public request names
		// its tenant by Host and nothing else. On any other group the tenant
		// comes from the caller's claims, so the flag exempts nothing and
		// reads as a claim about isolation that is not being made.
		if r.SelfScoping && r.Group != core.GroupPublic {
			t.Errorf("%s sets SelfScoping on a %s route; the flag only relaxes the public-route Host rule", where, r.Group)
		}

		// Only the authenticated group reads a declared scope, and a value
		// the engine cannot read closes the route to every key short of the
		// full wildcard. Either reads as a working declaration and is not.
		if r.Scope != "" {
			if r.Group != core.GroupAuth {
				t.Errorf("%s declares scope %q on a %s route; only authenticated routes read a declared scope", where, r.Scope, r.Group)
			} else if _, ok := core.ParseScope(r.Scope); !ok {
				t.Errorf("%s declares scope %q, which is not resource:action; only a key granted everything would reach it", where, r.Scope)
			}
		}

		// The public limiter wraps public routes only, and a bucket with no
		// rate or no burst refuses traffic instead of pacing it. The engine
		// skips either declaration, so the route keeps a limit the plugin
		// did not ask for.
		if rl := r.RateLimit; rl != nil {
			if r.Group != core.GroupPublic {
				t.Errorf("%s declares a rate limit on a %s route; the public limiter wraps only public routes, so the limit is ignored", where, r.Group)
			}
			if rl.Rate <= 0 || rl.Burst <= 0 {
				t.Errorf("%s declares a rate limit of %v requests a second with a burst of %d; both must be positive, or the limit is ignored", where, rl.Rate, rl.Burst)
			}
		}

		if r.Doc != nil {
			for _, msg := range docFaults(r.Pattern, r.Doc) {
				t.Errorf("%s: %s", where, msg)
			}
		}

		if err := core.ValidateAdminGrants([]core.RouteDecl{r}, routes...); err != nil {
			t.Errorf("%s: %v", where, err)
		}

		key := r.Method + " " + r.Pattern
		if prev, dup := seen[key]; dup {
			t.Errorf("%s is declared twice (%s); the second registration is unreachable", where, prev)
		}
		seen[key] = where
	}
}

// RequireNoSessionOnlyGrants fails for every route declaration whose admin
// grant names something outside the catalog, or sits on a route that
// core.SessionOnlyAdminPatterns covers or a declaration in routes marks
// SessionOnly. A plugin's route test calls it so a grant that would stop the
// admin router from building fails in the plugin's own suite first.
// AssertRouteContract runs the same check.
func RequireNoSessionOnlyGrants(t T, routes []core.RouteDecl) {
	t.Helper()
	for _, r := range routes {
		if err := core.ValidateAdminGrants([]core.RouteDecl{r}, routes...); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// paramLocations are the places OpenAPI lets a parameter live.
var paramLocations = map[string]bool{"path": true, "query": true, "header": true, "cookie": true}

// docFaults reports what in a route's Doc would publish a document a client
// cannot use: a parameter in no place OpenAPI knows, a path parameter the
// pattern does not hold, or a response under a number that is not an HTTP
// status.
func docFaults(pattern string, d *core.RouteDoc) []string {
	segments := map[string]bool{}
	for _, seg := range strings.Split(pattern, "/") {
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			name, _, _ := strings.Cut(seg[1:len(seg)-1], ":")
			segments[name] = true
		}
	}
	var out []string
	for _, p := range d.Params {
		switch {
		case p.Name == "":
			out = append(out, "documents a parameter with no name")
		case !paramLocations[p.In]:
			out = append(out, fmt.Sprintf("documents parameter %q in %q, which is not path, query, header or cookie", p.Name, p.In))
		case p.In == "path" && !segments[p.Name]:
			out = append(out, fmt.Sprintf("documents path parameter %q, which the pattern does not hold", p.Name))
		}
	}
	for status := range d.Responses {
		if status < 100 || status > 599 {
			out = append(out, fmt.Sprintf("documents a response under %d, which is not an HTTP status", status))
		}
	}
	sort.Strings(out)
	return out
}

// patternFault reports why a pattern is not one the router can mount, or "".
func patternFault(p string) string {
	switch {
	case p == "":
		return "the pattern is empty"
	case !strings.HasPrefix(p, "/"):
		return "the pattern does not start with /, so it is relative to nothing"
	case strings.Contains(p, "//"):
		return "the pattern holds an empty segment"
	case strings.Count(p, "{") != strings.Count(p, "}"):
		return "the pattern's braces are unbalanced"
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if strings.HasPrefix(seg, "{") && (len(seg) < 3 || !strings.HasSuffix(seg, "}")) {
			return fmt.Sprintf("segment %q is not a usable parameter", seg)
		}
	}
	return ""
}

// RoutePatterns returns the declared patterns, sorted and deduplicated. For a
// test that wants to state the surface a plugin serves rather than check it.
func RoutePatterns(routes []core.RouteDecl) []string {
	set := map[string]bool{}
	for _, r := range routes {
		set[r.Pattern] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
