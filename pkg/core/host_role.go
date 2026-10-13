package core

import (
	"errors"
	"fmt"
	"sync"
)

// HostRole is a service one plugin supplies to the engine and to every other
// plugin through a registrar on the host, such as the permission checker or
// the schema engine. One plugin holds a role at a time.
type HostRole string

// The roles a plugin supplies through ScopedHost. Each registrar needs the
// capability named beside it.
const (
	HostRolePermissionChecker       HostRole = "permission checker"        // CapPermissionChecker
	HostRoleSchemaEngine            HostRole = "schema engine"             // CapSchema
	HostRoleEmailSender             HostRole = "email sender"              // CapEmailSender
	HostRoleFlowDefinitionValidator HostRole = "flow definition validator" // CapFlowRegistry
	HostRoleFlowInvoker             HostRole = "flow invoker"              // CapFlowRegistry
	HostRoleContentLocalizer        HostRole = "content localizer"         // CapContentLocalizer
	HostRoleContentEntryWriter      HostRole = "content entry writer"      // CapContentEntryWriter
	HostRoleRecordRevisionStore     HostRole = "record revision store"     // CapRecordRevisionStore
	HostRoleStorage                 HostRole = "storage"                   // CapStorage
	HostRoleUserProvisioner         HostRole = "user provisioner"          // CapUserProvisioner
)

// ErrHostRoleHeld is returned when a plugin registers or clears a host role
// another plugin holds. The holder keeps the role until it clears it.
var ErrHostRoleHeld = errors.New("host role held by another plugin")

// ErrHostRoleNotHeld is returned when a plugin clears a host role it does not
// hold. Only the plugin that registered a role may take it away, and a clear
// from a plugin whose own registration never happened would otherwise remove
// whatever the engine installed in its place.
var ErrHostRoleNotHeld = errors.New("host role not held by this plugin")

// ErrHostRoleUnsupported is returned when the host cannot take a role at all:
// it has no registrar for it, or it keeps no record of who holds it. The
// engine host takes every role, so this is a test host or a host built for
// one purpose. A plugin can carry on without its role there, where every
// other refusal means the build or another plugin stands in its way.
var ErrHostRoleUnsupported = errors.New("host takes no such role")

// HostRoleOwnerRegistrar is implemented by the engine host. ScopedHost passes
// every role registration through it under the plugin's name once the
// plugin's grant allows it, so the host refuses a plugin that would replace
// or clear a role another plugin holds.
//
// set installs the plugin's value, or clears the role when clear is true. The
// host calls it only for a change it accepts, while it holds its record of
// the role's holder, so the record and the installed value cannot disagree.
type HostRoleOwnerRegistrar interface {
	RegisterOwnedRole(owner string, role HostRole, clear bool, set func() error) error
}

// HostRoleRegistry records the plugin that holds each host role. A host
// embeds one to implement HostRoleOwnerRegistrar. The zero value is ready to
// use.
type HostRoleRegistry struct {
	mu      sync.Mutex
	holders map[HostRole]string
}

var _ HostRoleOwnerRegistrar = (*HostRoleRegistry)(nil)

// RegisterOwnedRole runs set for owner and records the result. A registration
// makes owner the holder, and a clear ends its hold. A role another owner
// holds is refused with ErrHostRoleHeld, and a clear by an owner that holds
// nothing with ErrHostRoleNotHeld. An error from set leaves the record as it
// was.
func (r *HostRoleRegistry) RegisterOwnedRole(owner string, role HostRole, clear bool, set func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	holder, held := r.holders[role]
	if held && holder != owner {
		return fmt.Errorf("%w: %s holds the %s", ErrHostRoleHeld, holder, role)
	}
	if clear && !held {
		return fmt.Errorf("%w: %s does not hold the %s", ErrHostRoleNotHeld, owner, role)
	}
	if err := set(); err != nil {
		return err
	}
	if clear {
		delete(r.holders, role)
		return nil
	}
	if r.holders == nil {
		r.holders = map[HostRole]string{}
	}
	r.holders[role] = owner
	return nil
}
