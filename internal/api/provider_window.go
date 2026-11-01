package api

import (
	"math"
	"net/http"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// providerWindowHandler serves a tenant's provider window: how many days after
// its provider last signed an account in that account may still sign in by
// password, password reset or magic link.
//
// The window is one key in the tenant's engine settings row. An admin acts on
// the tenant their session acts in, and a super admin on any tenant through
// X-Tenant-ID, because the tenancy middleware puts exactly that tenant on the
// request. Only a signed-in session may call it: the setting decides who can
// get in.
type providerWindowHandler struct {
	store   *db.PluginConfigStore
	windows *config.ProviderWindows
	host    core.Host
}

type providerWindowBody struct {
	Days int `json:"days"`
}

// get answers the window of the request's tenant.
// GET /api/admin/auth/provider-window
func (h *providerWindowHandler) get(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.caller(w, r)
	if !ok {
		return
	}
	days, err := h.windows.ProviderWindowDays(r.Context(), tenantID)
	if err != nil {
		httpx.StoreError(w, r, err, "failed to read the provider window")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, http.StatusOK, providerWindowBody{Days: days})
}

// put sets the window of the request's tenant. It applies to the next
// password login, reset or magic link, with no restart, and writes one audit
// row naming the old and new values.
// PUT /api/admin/auth/provider-window
// Body: {"days": N}, a whole number from 0 to 365.
func (h *providerWindowHandler) put(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body map[string]any
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil || body == nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "body must be a JSON object with a days field")
		return
	}
	raw, isNumber := body["days"].(float64)
	if !isNumber || raw != math.Trunc(raw) || raw < 0 || raw > core.ProviderWindowMaxDays {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, "days must be a whole number from 0 to 365")
		return
	}
	days := int(raw)

	// Merged into the stored row, which holds the tenant's other engine
	// settings, under a lock so a concurrent save of another key survives. The
	// audited old value is the one read under that lock, so two overlapping
	// saves each name the value they replaced.
	prior, err := h.store.Merge(r.Context(), config.CoreSettingsOwner,
		map[string]any{core.ProviderWindowSettingKey: days}, actorID(r))
	if err != nil {
		httpx.StoreError(w, r, err, "failed to save the provider window")
		return
	}
	compliance.RecordAuditFromRequestWithState(r, h.host, "auth.provider_window.update", "tenant", tenantID,
		map[string]any{"tenant_id": tenantID, "days": config.ProviderWindowDaysIn(prior)},
		map[string]any{"tenant_id": tenantID, "days": days})

	w.Header().Set("Cache-Control", "no-store")
	respond(w, http.StatusOK, providerWindowBody{Days: days})
}

// caller returns the tenant the request acts in, or writes the refusal. An
// API key or an admin token is refused: the window decides who can get in.
func (h *providerWindowHandler) caller(w http.ResponseWriter, r *http.Request) (string, bool) {
	if ac := core.GetClaims(r.Context()); ac != nil && (ac.IsAPIKey || ac.AdminTokenID != "") {
		httpx.ErrorReq(w, r, http.StatusForbidden, "The provider window is managed from a signed-in session.")
		return "", false
	}
	if claimsFromCtx(r) == nil {
		httpx.ErrorReq(w, r, http.StatusForbidden, "The provider window is managed from a signed-in session.")
		return "", false
	}
	return tenantOf(w, r)
}
