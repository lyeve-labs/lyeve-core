package core

import (
	"context"

	"github.com/google/uuid"
)

// UserProvisioner provisions an account just in time, at single sign-on.
// FindOrCreateUser returns the account for an identity that the identity
// provider idpID asserted, and creates it when none exists yet. An error
// leaves the caller to find or create the account itself.
type UserProvisioner interface {
	FindOrCreateUser(ctx context.Context, idpID uuid.UUID, email string, roles []string) (userID uuid.UUID, emailOut string, assignedRoles []string, err error)
}

// UserProvisionerSetter is implemented by the engine host. A plugin that
// keeps a directory of provisioned accounts registers its provisioner
// through it.
type UserProvisionerSetter interface {
	SetUserProvisioner(p UserProvisioner)
}

// UserProvisionerProvider is implemented by the engine host. A plugin that
// signs users in through an identity provider reads the registered
// provisioner from it, and gets nil when no plugin registered one.
type UserProvisionerProvider interface {
	UserProvisioner() UserProvisioner
}

// UserProvisioner forwards to inner if it implements UserProvisionerProvider,
// and answers nil otherwise.
func (h *ScopedHost) UserProvisioner() UserProvisioner {
	if p, ok := h.inner.(UserProvisionerProvider); ok {
		return p.UserProvisioner()
	}
	return nil
}

// SetUserProvisioner forwards to inner if it implements UserProvisionerSetter.
func (h *ScopedHost) SetUserProvisioner(p UserProvisioner) {
	if s, ok := h.inner.(UserProvisionerSetter); ok {
		s.SetUserProvisioner(p)
	}
}
