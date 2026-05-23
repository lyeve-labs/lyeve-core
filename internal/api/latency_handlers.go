package api

import (
	"net/http"
	"strconv"

	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// latencyStatsHandler returns per-endpoint latency statistics ranked by p95.
// GET /api/admin/debug/latency
// Auth: admin or super_admin.
func latencyStatsHandler(tracker *middleware.LatencyTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := 20
		if topStr := r.URL.Query().Get("top"); topStr != "" {
			if parsed, err := strconv.Atoi(topStr); err == nil && parsed > 0 && parsed <= 1000 {
				n = parsed
			}
		}

		var result any
		if tracker == nil {
			result = map[string]any{
				"message": "latency tracker not configured",
				"slowest": nil,
			}
		} else {
			slowest := tracker.Slowest(n)
			ts := tracker.Stats()
			result = map[string]any{
				"tracker": ts,
				"slowest": slowest,
				"total":   len(slowest),
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = jsonpool.WriteJSON(w, result) // err suppressed: response write to client
	}
}
