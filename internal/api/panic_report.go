package api

import (
	"net/http"
	"runtime/debug"

	"github.com/go-chi/chi/v5"

	"github.com/lyeve-labs/lyeve-core/internal/panicreport"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// reportPanics hands a panic raised below it to the panic reporter and raises
// it again, so middleware.Recoverer above still logs it and answers 500. It
// is mounted directly under the recoverer, so a plugin handler's panic passes
// through it like the engine's own.
//
// The report names the route pattern, never the path, which can carry a
// token, and nothing of the request's body, headers or query. The tenant is
// read from the slot the access logger installs, because the request this
// frame holds was made before tenancy was resolved. http.ErrAbortHandler is
// how a handler ends a response early on purpose, so it is passed on
// unreported.
func reportPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rv := recover()
			if rv == nil {
				return
			}
			if rv != http.ErrAbortHandler { //nolint:errorlint // recover returns the value panic was given, never a wrapped error
				p := panicreport.Panic{
					TenantID: core.TenantSlotFrom(r.Context()).Get(),
					Method:   r.Method,
					Value:    rv,
					Stack:    debug.Stack(),
				}
				if rctx := chi.RouteContext(r.Context()); rctx != nil {
					p.Route = rctx.RoutePattern()
				}
				panicreport.Report(p)
			}
			panic(rv)
		}()
		next.ServeHTTP(w, r)
	})
}
