package api

import (
	"net/http"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// SecurityControlsProvider is anything that can say which security controls
// are enforcing right now. The runtime passes itself. Tests pass a stub.
type SecurityControlsProvider interface {
	SecurityControls() []core.SecurityControl
}

// securityControlsResponse is the report as the admin reads it: each control
// as its plugin evaluated it, with what an operator does about one that is
// not enforcing.
type securityControlsResponse struct {
	CheckedAt time.Time              `json:"checked_at"`
	Controls  []core.SecurityControl `json:"controls"`
	Failures  int                    `json:"failures"`
}

// securityControlsHandler serves the control liveness report.
// GET /api/admin/security/controls
// Auth: super_admin.
//
// The same report the runtime logs at boot, read live: a plugin activated
// by a license change after boot shows here without a restart.
func securityControlsHandler(provider SecurityControlsProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		if provider == nil {
			httpx.ErrorReq(w, r, http.StatusNotFound, "security controls not available")
			return
		}
		rows := provider.SecurityControls()
		out := securityControlsResponse{
			CheckedAt: time.Now().UTC(),
			Controls:  make([]core.SecurityControl, 0, len(rows)),
		}
		for _, row := range rows {
			// A control that is enforcing needs nothing done, whatever its
			// reporter sent.
			if row.Status == core.ControlPass {
				row.Remedy = ""
			}
			if row.Status == core.ControlFail {
				out.Failures++
			}
			out.Controls = append(out.Controls, row)
		}
		_ = jsonpool.WriteJSON(w, out) // err suppressed: response write to client
	}
}

// robotsHandler refuses every crawler.
// GET /robots.txt
// Auth: none. A crawler carries no token, and the answer is the same for all.
func robotsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
}
