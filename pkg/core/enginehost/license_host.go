package enginehost

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// Capabilities returns the licensing implementation's snapshot of the
// instance's entitlement, copied into the set every plugin gate reads. What a
// license grants, and what an install keeps without one, is the
// implementation's to decide, so nothing is added here.
//
// A host with no manager wired grants nothing. The runtime wires one before
// any plugin starts, so only a host built outside the runtime answers this
// way, and it has no license to read.
func (h *engineHost) Capabilities() core.CapabilitySet {
	if h.licensing == nil {
		return core.CapabilitySet{Features: map[string]bool{}, Plan: "free", State: "free"}
	}
	snap := h.licensing.Snapshot()
	caps := core.CapabilitySet{
		Features:    make(map[string]bool, len(snap.Features)),
		Plan:        snap.Plan,
		State:       snap.State,
		TenantQuota: snap.TenantQuota,
	}
	for _, f := range snap.Features {
		caps.Features[f] = true
	}
	if len(snap.Caps) > 0 {
		caps.Caps = make(map[string]int, len(snap.Caps))
		for name, limit := range snap.Caps {
			caps.Caps[name] = limit
		}
	}
	return caps
}

// WithLicensing wires the licensing implementation's manager into the engine
// host, so Capabilities and HasFeature answer from the license in force.
// Called once at boot by the runtime before plugins start.
func (h *engineHost) WithLicensing(mgr licensing.Manager) {
	h.licensing = mgr
}

// HasFeature reports whether the instance is entitled to the named feature
// and the request's tenant is not refused it.
//
// It answers from the same snapshot Capabilities copies, because both are on
// core.LicenseHost and a plugin started as entitled must not be told by the
// other half of the interface that it is not.
//
// A feature an operator withheld from the request's tenant is refused before
// that, whatever the snapshot says: the license is the instance's ceiling and
// the withheld set narrows it per tenant. A call with no tenant on the
// context, such as a background job, is answered for the instance.
func (h *engineHost) HasFeature(ctx context.Context, feature string) bool {
	if h.licensing != nil && h.licensing.Withholds(core.TenantIDFromCtx(ctx), feature) {
		return false
	}
	return h.Capabilities().Features[feature]
}
