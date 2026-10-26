package api

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// heapExportRefusedKey is the setting that refuses the heap and allocs
// profiles. The profiler plugin reads the same key for its own export route,
// so one setting governs every path a memory profile leaves by.
const heapExportRefusedKey = "profiler_no_heap_export"

// memoryProfileGuard serves a heap or allocs profile only while the operator
// has not refused memory profile exports, and logs each one it serves. Those
// profiles are drawn from the process's live allocations, which is why an
// operator can refuse them outright. The setting is read on every request,
// so a change saved in the console applies to the next one.
func memoryProfileGuard(profile string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refused, _ := strconv.ParseBool(config.ActiveResolver().Get(heapExportRefusedKey)); refused {
			httpx.ErrorReq(w, r, http.StatusForbidden, "heap profile export is disabled")
			return
		}
		userID := "unknown"
		if claims := core.GetClaims(r.Context()); claims != nil {
			userID = claims.UserID
		}
		slog.WarnContext(r.Context(), "pprof: heap profile exported",
			"type", profile,
			"user_id", userID,
			"remote_addr", r.RemoteAddr,
		)
		next.ServeHTTP(w, r)
	})
}
