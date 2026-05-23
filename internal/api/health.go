package api

import (
	"context"
	"net/http"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// Health handlers

// healthHandlerFn returns the liveness probe handler.
// GET /api/admin/health
// GET /api/v1/health
func healthHandlerFn(pool db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "database unreachable")
			return
		}
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// readyHandlerFn returns the readiness probe handler.
// GET /api/admin/ready
// GET /api/v1/ready
//
// cacheLen counts the schemas the engine has loaded. A deployment gate reads
// schema_count, so it must be real: a 0 reads as "not ready yet".
// schemaEnginePresent is optional and variadic so the existing callers keep
// compiling. Omitted, readiness says nothing about the schema engine. Given,
// it reports whether this install can describe content types.
func readyHandlerFn(pool db.DB, cacheLen func() int, schemaEnginePresent ...func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "database unreachable")
			return
		}

		body := map[string]any{
			"status": "ready",
			"db":     "ok",
		}
		// Absent rather than zero when nothing counts. Reporting 0 for a
		// number no one collects is indistinguishable from a true zero, and
		// this one is read by deployment gates.
		if cacheLen != nil {
			body["schema_count"] = cacheLen()
		}

		// An install with no schema engine still serves, so this stays a 200
		// and the process stays in rotation. It does not call itself ready,
		// because it cannot do the thing it exists for, and an operator
		// reading a bare "ready" would have no way to find that out.
		if len(schemaEnginePresent) > 0 && schemaEnginePresent[0] != nil && !schemaEnginePresent[0]() {
			body["status"] = "degraded"
			body["schema_engine"] = "absent"
			body["limitations"] = []string{
				"content types cannot be defined, changed or served",
			}
		}

		respond(w, http.StatusOK, body)
	}
}

// schemaCount counts the definitions an install can serve, for the readiness
// body. It counts through core.SchemaSource, so the router never reaches into
// a plugin's cache.
//
// An install with no schema engine counts nothing, and readiness reports the
// absence separately, so a zero here is never read as an outage.
func schemaCount(src core.SchemaSource) func() int {
	return func() int {
		defs, err := src.List(context.Background())
		if err != nil {
			return 0
		}
		return len(defs)
	}
}
