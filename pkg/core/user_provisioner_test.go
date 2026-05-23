package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

type namedProvisioner struct{ name string }

func (p *namedProvisioner) FindOrCreateUser(context.Context, uuid.UUID, string, []string) (uuid.UUID, string, []string, error) {
	return uuid.Nil, p.name, nil, nil
}

// userProvisionerHost keeps the provisioner slot.
type userProvisionerHost struct {
	stubHost
	p UserProvisioner
}

func (h *userProvisionerHost) SetUserProvisioner(p UserProvisioner) { h.p = p }
func (h *userProvisionerHost) UserProvisioner() UserProvisioner     { return h.p }

func TestScopedHost_UserProvisioner_ForwardsToInner(t *testing.T) {
	inner := &userProvisionerHost{}
	h := NewScopedHost(inner, "directory", CapAll)
	p := &namedProvisioner{name: "directory"}

	h.SetUserProvisioner(p)

	assert.Same(t, p, inner.p)
	assert.Same(t, p, h.UserProvisioner())
}

func TestScopedHost_UserProvisioner_NilWhenInnerHasNoSlot(t *testing.T) {
	h := NewScopedHost(&stubHost{}, "sso", 0)

	h.SetUserProvisioner(&namedProvisioner{})

	assert.Nil(t, h.UserProvisioner())
}
