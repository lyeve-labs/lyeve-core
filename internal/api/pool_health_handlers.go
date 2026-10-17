package api

import (
	"log/slog"
	"net/http"

	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// poolHealthHandler returns connection pool statistics.
// GET /api/admin/pool/health
// Auth: admin or super_admin.
func poolHealthHandler(provider observability.PoolHealthProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			respond(w, http.StatusOK, map[string]any{
				"healthy": true,
				"message": "pool health not configured - enable CONNECTION_POOLER to activate",
			})
			return
		}

		// The snapshot is encoded in full before anything is written. A value
		// JSON cannot hold, such as a NaN latency from a provider, then fails
		// the encode alone, and the answer is still free to be the error
		// envelope.
		body, err := jsonpool.MarshalJSON(provider.PoolHealth(r.Context()))
		if err != nil {
			slog.ErrorContext(r.Context(), "pool health: encode snapshot", "err", err)
			httpx.ErrorReq(w, r, http.StatusInternalServerError, "failed to encode response")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) // err suppressed: response write to client
	}
}
