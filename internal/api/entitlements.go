package api

import (
	"net/http"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// entitlementsBody is the snapshot with what the caller's tenant is refused
// beside it.
type entitlementsBody struct {
	Plan string `json:"plan"`
	// PlanLabel is the plan as an operator reads it. See licensing.Snapshot.
	PlanLabel   string   `json:"plan_label,omitempty"`
	State       string   `json:"state"`
	Features    []string `json:"features"`
	TenantQuota int      `json:"tenant_quota"`
	// Source is what renews the license. See licensing.Snapshot.
	Source    string     `json:"license_source,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// GraceEndsAt is when the licensed features stop, set only in grace.
	GraceEndsAt *time.Time `json:"grace_ends_at,omitempty"`
	// Problem is the operator-facing reason the license is not what was
	// configured. It never carries the key or the token.
	Problem string `json:"license_error,omitempty"`
	// Withheld names what an operator took away from the caller's tenant.
	// Features is the license and stays the instance's answer. The admin
	// subtracts this list so a tenant is not shown screens its requests
	// would be refused.
	Withheld []string `json:"withheld"`
	// Caps is every capacity ceiling this install runs under, by name, where
	// 0 is unlimited. The admin renders current against ceiling from this and
	// the count the list endpoint returns, and holds no ceiling of its own: a
	// console that knew the number would keep enforcing one the license had
	// lifted.
	//
	// It comes from the same snapshot the gates read, so the answer an
	// operator reads cannot drift away from the refusal that stops them.
	Caps map[string]int `json:"caps"`
	// LicenseModule says whether the build links a licensing implementation
	// that verifies licenses. The engine answers it from what the build
	// linked, never from the license, and never omits it, so a console can
	// tell an engine too old to send it by its absence.
	LicenseModule bool `json:"license_module"`
}

// entitlementsHandler returns the active plan, entitled features, and capacity
// caps, and whether the build links a licensing implementation.
// GET /api/admin/entitlements
// Auth: admin or super_admin.
func entitlementsHandler(ent EntitlementProvider, licenseModule bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		snap := ent.Snapshot()
		body := entitlementsBody{
			Plan:          snap.Plan,
			PlanLabel:     snap.PlanLabel,
			State:         snap.State,
			Features:      snap.Features,
			TenantQuota:   snap.TenantQuota,
			Source:        snap.Source,
			ExpiresAt:     snap.ExpiresAt,
			GraceEndsAt:   snap.GraceEndsAt,
			Problem:       snap.Problem,
			Withheld:      ent.WithheldFrom(core.TenantIDFromCtx(r.Context())),
			Caps:          snap.Caps,
			LicenseModule: licenseModule,
		}
		// The admin reads each of these as a list or an object. An
		// implementation that left one nil would break every such read, so
		// the endpoint answers an empty one instead.
		if body.Features == nil {
			body.Features = []string{}
		}
		if body.Withheld == nil {
			body.Withheld = []string{}
		}
		if body.Caps == nil {
			body.Caps = map[string]int{}
		}
		_ = jsonpool.WriteJSON(w, body) // err suppressed: response write to client
	}
}
