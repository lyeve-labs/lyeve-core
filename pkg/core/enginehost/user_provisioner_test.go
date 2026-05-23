package enginehost

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type stubUserProvisioner struct{ name string }

func (p *stubUserProvisioner) FindOrCreateUser(context.Context, uuid.UUID, string, []string) (uuid.UUID, string, []string, error) {
	return uuid.Nil, p.name, nil, nil
}

// The host has one provisioner slot. A registration replaces the one before,
// and a plugin clearing it on its way out leaves nothing behind.
func TestEngineHost_UserProvisioner_OneSlot(t *testing.T) {
	h := &engineHost{}
	if h.UserProvisioner() != nil {
		t.Fatal("a host nobody registered with must answer nil")
	}

	first := &stubUserProvisioner{name: "first"}
	h.SetUserProvisioner(first)
	if got := h.UserProvisioner(); got != core.UserProvisioner(first) {
		t.Errorf("UserProvisioner() = %v, want the registered provisioner", got)
	}

	second := &stubUserProvisioner{name: "second"}
	h.SetUserProvisioner(second)
	if got := h.UserProvisioner(); got != core.UserProvisioner(second) {
		t.Errorf("UserProvisioner() after a second registration = %v, want the second provisioner", got)
	}

	h.SetUserProvisioner(nil)
	if h.UserProvisioner() != nil {
		t.Error("clearing the slot must leave nothing behind")
	}
}

// Plugins reach the host through their own scoped hosts. A directory plugin
// registering its provisioner is found by a sign-on plugin, and so is the
// one that replaces it.
func TestEngineHost_UserProvisioner_SharedAcrossScopedHosts(t *testing.T) {
	h := &engineHost{}
	directory := core.NewScopedHost(h, "directory", core.CapAll)
	signOn := core.NewScopedHost(h, "sign-on", core.CapAll)

	p := &stubUserProvisioner{name: "directory"}
	directory.SetUserProvisioner(p)
	if got := signOn.UserProvisioner(); got != core.UserProvisioner(p) {
		t.Errorf("UserProvisioner() through another plugin = %v, want the one the directory registered", got)
	}

	q := &stubUserProvisioner{name: "replacement"}
	directory.SetUserProvisioner(q)
	if got := signOn.UserProvisioner(); got != core.UserProvisioner(q) {
		t.Errorf("UserProvisioner() through another plugin = %v, want the replacement", got)
	}
}
