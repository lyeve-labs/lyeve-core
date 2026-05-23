package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// rosterLogInterval bounds how often a failing roster read is reported. The
// guard runs on every unresolved public request, so a database that cannot
// answer would otherwise write a line per request on top of whatever else is
// already failing.
const rosterLogInterval = time.Minute

// rosterLogGate throttles the guard's failure log.
type rosterLogGate struct {
	mu   sync.Mutex
	next time.Time
}

func (g *rosterLogGate) allow(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.next) {
		return false
	}
	g.next = now.Add(rosterLogInterval)
	return true
}

// RequirePublicTenant refuses a public request that names no tenant on an
// install that has more than one.
//
// A public request names its tenant by the Host header and by nothing else.
// The alternative is letting the caller name it, and an unauthenticated caller
// that can name its own tenant can read any tenant's public surface by
// guessing a slug, which is not a secret: it is in the URL of every page that
// tenant publishes. Routing decides the tenant. The caller does not.
//
// The guard fires on one line of the table and only one:
//
//	tenants  host rostered   answer
//	0        n/a             served unresolved, so first-run setup still answers
//	1        either          the sole tenant, resolved before this runs
//	1        unresolved      503, the resolver failed to answer, so retry
//	2+       yes             that tenant, resolved before this runs
//	2+       no              404, because nobody claimed this host
//	unknown  unresolved      503, the roster could not be read, so retry
//
// So an empty tenant here is not on its own a refusal. With no tenants the
// install has not been provisioned and the request has to be served. With one
// the resolver upstream answers. When it did not, serving the request with no
// tenant is not safe, because several public stores read an empty tenant as
// the implicit one or drop the tenant predicate. The 2+ line is ambiguous in
// the direction that matters: serving it would mean picking one of several
// tenants for a hostname none of them registered.
//
// A 404 rather than a 403 because that is the honest answer. A host that
// resolves no tenant was never rostered, so this install serves nothing for
// it, and whatever sits in front of it owns that name. It also says nothing a
// caller did not already know, which a 403 would: refusing differently for a
// tenant that exists than for one that does not is the enumeration oracle the
// rule exists to close.
//
// nil fn disables the guard. That is the single-tenant engine and any install
// with no tenant roster provider, where there is no roster to consult and the
// question does not arise.
func RequirePublicTenant(multiTenant bool, fn core.TenantRosterFunc) func(http.Handler) http.Handler {
	if !multiTenant || fn == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	gate := &rosterLogGate{}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if TenantIDFromCtx(r) != "" {
				next.ServeHTTP(w, r)
				return
			}

			count, err := fn(r.Context())
			if err != nil {
				// Not a 404, which would read as the content having been
				// deleted, and not served either: without the roster there is
				// no telling whether this is first-run setup or a host on a
				// provisioned install, and the stores behind public routes do
				// not all refuse an empty tenant. A retryable 503 is true.
				if gate.allow(time.Now()) {
					slog.WarnContext(r.Context(), "could not read the tenant roster, refusing the public request",
						"path", r.URL.Path, "error", err)
				}
				refuseUnresolved(w)
				return
			}
			if count == 0 {
				next.ServeHTTP(w, r)
				return
			}
			if count == 1 {
				// The resolver upstream names the sole tenant. Reaching here
				// means it failed to, and the request would otherwise run as
				// the implicit tenant or as none.
				refuseUnresolved(w)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not found"}`)
		})
	}
}

func refuseUnresolved(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, `{"error":"tenant could not be resolved"}`)
}
