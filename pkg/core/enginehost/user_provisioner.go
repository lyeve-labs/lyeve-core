package enginehost

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// The host keeps one provisioner slot. The plugin that provisions accounts
// fills it and the plugins that sign users in read it.
var (
	_ core.UserProvisionerSetter   = (*engineHost)(nil)
	_ core.UserProvisionerProvider = (*engineHost)(nil)
)

// SetUserProvisioner implements core.UserProvisionerSetter. Called by the
// plugin that supplies just-in-time provisioning, from its Start.
func (h *engineHost) SetUserProvisioner(p core.UserProvisioner) { h.userProvisioner = p }

// UserProvisioner implements core.UserProvisionerProvider. Returns the
// registered provisioner, or nil when no plugin has registered one.
func (h *engineHost) UserProvisioner() core.UserProvisioner { return h.userProvisioner }
