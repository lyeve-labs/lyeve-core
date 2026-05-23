package middleware

import (
	"net/http"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// RequireScoped checks API key scopes against the requested endpoint. Only
// gates X-API-Key authenticated requests. JWT requests pass through. The
// required scope is derived from method+path via core.BuildScopeRoute
// and checked via core.ScopeGrants. Fail-closed: keys with no scopes
// are denied. Wildcard scope "*:*" permits all endpoints.
//
// A key that carries a privileged role (admin or super_admin) is role-based
// rather than scope-based, mirroring the access a session holding that role
// gets, so it is not scope-gated.
func RequireScoped(next http.Handler) http.Handler {
	return RequireScope(PathScope)(next)
}

// PathScope is the scope the request's method and path name.
func PathScope(r *http.Request) (core.ScopePair, bool) {
	return core.BuildScopeRoute(r.Method, r.URL.Path), true
}

// RequireScope holds an API key to the scope required answers for the
// request, on the terms RequireScoped describes. When required reports
// false the route's scope could not be read, and only a key granted
// everything passes, so a scope nobody can parse never opens a route.
func RequireScope(required func(*http.Request) (core.ScopePair, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := core.GetClaims(r.Context())
			if claims == nil {
				// No API-key claims: let JWT middleware handle 401.
				next.ServeHTTP(w, r)
				return
			}

			if !claims.IsAPIKey {
				next.ServeHTTP(w, r)
				return
			}

			// A privileged role grants the key the same reach a session with
			// that role has, without a scope. Otherwise the scope matrix
			// applies.
			if claims.HasRole("admin") || claims.HasRole("super_admin") {
				next.ServeHTTP(w, r)
				return
			}

			want, ok := required(r)
			granted := core.ScopeGrantsAll(claims.Scopes)
			if ok && !granted {
				granted = core.ScopeGrants(claims.Scopes, want.Resource, want.Action)
			}
			if !granted {
				http.Error(w, `{"error":"insufficient scope"}`, http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
