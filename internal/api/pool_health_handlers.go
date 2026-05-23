package api

import (
	"net/http"

	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
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

		snapshot := provider.PoolHealth(r.Context())
		w.Header().Set("Content-Type", "application/json")
		if err := jsonpool.WriteJSON(w, snapshot); err != nil {
			http.Error(w, `{"error":"failed to encode response"}`, http.StatusInternalServerError)
		}
	}
}
