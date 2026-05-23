package api

import (
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// EntitlementProvider is what the routers read the license through: the
// instance's entitlement for the entitlements endpoint, what a tenant is
// refused for that endpoint and for the plugin route guard, and which plugins
// start whatever the license says for the document. The licensing
// implementation's manager satisfies it. applyRouterOptions substitutes
// unlicensedEntitlements when none is wired, so a handler never sees a nil
// provider and always fails closed to the unlicensed set.
type EntitlementProvider interface {
	// Snapshot is the finished entitlement of the instance.
	Snapshot() licensing.Snapshot
	// Withholds reports whether name is refused to tenant.
	Withholds(tenant, name string) bool
	// WithheldFrom is every name refused to tenant, sorted and never nil.
	WithheldFrom(tenant string) []string
	// Plugin is the grant of the compiled plugin name. The routes of a
	// plugin its grant marks ungated need no license feature.
	Plugin(name string) licensing.PluginGrant
}

// WithEntitlements injects the licensing implementation's manager. When
// unset, the routers fall back to unlicensed behavior.
func WithEntitlements(p EntitlementProvider) RouterOption {
	return func(o *routerOptions) { o.entitlements = p }
}

// WithLicenseModule says whether the build links a licensing implementation
// that verifies licenses, which the entitlements endpoint reports as
// license_module. It is a property of what was compiled in, so the runtime
// passes it from the verifier it was handed. Unset, the endpoint reports
// false.
func WithLicenseModule(linked bool) RouterOption {
	return func(o *routerOptions) { o.licenseModule = linked }
}

// unlicensedEntitlements is the EntitlementProvider used when none is wired
// (the NewRouter compatibility path and tests). It grants no feature and
// withholds nothing, so a missing license never enables a feature by accident.
type unlicensedEntitlements struct{}

// Snapshot is the unlicensed snapshot: no feature and no ceiling, because a
// ceiling is something a licensing implementation states.
func (unlicensedEntitlements) Snapshot() licensing.Snapshot {
	return licensing.Snapshot{
		Plan:     "free",
		State:    "free",
		Features: []string{},
		Caps:     map[string]int{},
	}
}

// Withholds refuses nothing, because nothing is granted.
func (unlicensedEntitlements) Withholds(string, string) bool { return false }

// WithheldFrom is empty for every tenant.
func (unlicensedEntitlements) WithheldFrom(string) []string { return []string{} }

// Plugin refuses every plugin, so each plugin route names the feature it
// needs.
func (unlicensedEntitlements) Plugin(string) licensing.PluginGrant { return licensing.PluginGrant{} }
