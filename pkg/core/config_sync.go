package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Configuration sync moves an instance's configuration to another instance
// in one bundle: content types, flows, permission rules, webhooks and a named
// set of settings. Each kind of configuration belongs to the plugin that
// stores it, so the plugin that assembles the bundle never reads another
// plugin's tables. Every owner registers a ConfigSection on the host, and the
// assembling plugin reads the registered sections per request.
//
// A section is the owner's own description of its resources. The kernel
// defines the contract and holds the registry. It never reads inside a
// section.

// ConfigAction is what applying a bundle does to one resource.
type ConfigAction string

const (
	// ConfigCreate means the target holds no resource by this key.
	ConfigCreate ConfigAction = "create"
	// ConfigUpdate means the target holds the resource and it differs.
	ConfigUpdate ConfigAction = "update"
	// ConfigDelete means the target holds a resource the bundle does not
	// name, and the caller asked for such resources to be removed.
	ConfigDelete ConfigAction = "delete"
	// ConfigUnchanged means the target already matches the bundle.
	ConfigUnchanged ConfigAction = "unchanged"
)

// ConfigChange is one resource's line in a plan.
type ConfigChange struct {
	// Key names the resource the way the bundle does: a flow's slug, a
	// webhook's name, a rule's role and resource.
	Key    string       `json:"key"`
	Action ConfigAction `json:"action"`
	// Fields names the top-level fields an update changes. A changed secret
	// is listed by its field name, never by its value.
	Fields []string `json:"fields,omitempty"`
	// Note says something the action alone does not, such as a resource that
	// is kept on purpose.
	Note string `json:"note,omitempty"`
}

// ConfigProblem is a reason a section refuses a bundle. A plan with any
// problem is never applied.
type ConfigProblem struct {
	Key     string `json:"key,omitempty"`
	Message string `json:"message"`
	// Feature is the capability that would lift the problem, in its
	// "feature:<name>" form, when a license is what stands in the way.
	Feature string `json:"feature,omitempty"`
}

// ConfigPlan is a section's answer to a bundle: what applying it would
// change, and what stops it from being applied.
type ConfigPlan struct {
	Changes  []ConfigChange  `json:"changes"`
	Problems []ConfigProblem `json:"problems,omitempty"`
}

// ConfigSealer seals a secret into a bundle and opens one from it. The
// assembling plugin derives it from a passphrase the caller gives, so a
// bundle at rest carries no secret in the clear and no key of the instance
// that wrote it. A section seals every secret it exports and opens every
// secret it applies, then stores the plaintext under its own instance's key.
type ConfigSealer interface {
	Seal(plaintext string) (string, error)
	Open(sealed string) (string, error)
}

// ConfigOptions are the caller's choices for one export, plan or apply.
type ConfigOptions struct {
	// Sealer seals and opens the bundle's secrets. Never nil when a section
	// is called.
	Sealer ConfigSealer
	// Prune removes resources the target holds and the bundle does not name.
	// Without it they are left alone and do not appear in the plan.
	Prune bool
}

// ConfigApplied is what a section wrote inside the apply transaction.
type ConfigApplied struct {
	Changes []ConfigChange
	// AfterCommit runs once the transaction holding every section's writes
	// has committed, for the section to drop a cache or remount what it
	// serves. Nil when there is nothing to refresh. It never runs after a
	// rollback.
	AfterCommit func(ctx context.Context)
}

// ConfigSection is one kind of configuration a plugin owns.
//
// ExportConfig and PlanConfig read through the plugin's own querier.
// ApplyConfig reads and writes through tx alone: every section's writes share
// one transaction, so a section that touched the database another way would
// see the state before the apply, and on SQL Server it would wait on the
// transaction's own locks. ApplyConfig is only called with a bundle whose
// plan carried no problem, and it re-reads the target through tx rather than
// trusting that plan. A failed write returns a *ConfigApplyError naming the
// resource.
//
// Every call is scoped to the tenant on ctx, the way the plugin's own routes
// are.
type ConfigSection interface {
	// ConfigSectionName is the section's key in a bundle, such as "flows".
	ConfigSectionName() string
	ExportConfig(ctx context.Context, opts ConfigOptions) (json.RawMessage, error)
	PlanConfig(ctx context.Context, data json.RawMessage, opts ConfigOptions) (ConfigPlan, error)
	ApplyConfig(ctx context.Context, tx Querier, data json.RawMessage, opts ConfigOptions) (ConfigApplied, error)
}

// ConfigApplyError is a write a section could not make. Key names the
// resource, so a caller can report which one stopped the apply.
type ConfigApplyError struct {
	Key string
	Err error
}

func (e *ConfigApplyError) Error() string {
	return fmt.Sprintf("apply %s: %v", e.Key, e.Err)
}

func (e *ConfigApplyError) Unwrap() error { return e.Err }

// ErrConfigUnreadable is returned by PlanConfig or ApplyConfig for section
// data that does not parse, or a sealed secret the sealer cannot open. The
// caller answers it as a client error.
var ErrConfigUnreadable = errors.New("config section unreadable")

// ConfigSectionRegistrar is implemented by the engine host. A plugin
// registers its section when it starts and passes nil under the same name
// when it stops, so the section follows the plugin's lifecycle.
type ConfigSectionRegistrar interface {
	RegisterConfigSection(name string, s ConfigSection)
}

// ConfigSectionProvider is implemented by the engine host and forwarded by
// ScopedHost. It is read per request, never at Start, because sections arrive
// as their plugins start.
type ConfigSectionProvider interface {
	ConfigSections() []ConfigSection
}

// ConfigSectionOwnerRegistrar is implemented by the engine host. ScopedHost
// registers a plugin's section through it under the plugin's name, so the
// registry can refuse a name another owner holds.
type ConfigSectionOwnerRegistrar interface {
	RegisterOwnedConfigSection(owner, name string, s ConfigSection) error
}

// ErrConfigSectionOwned is returned when a registration names a section
// another owner holds. The first owner keeps it until it passes nil.
var ErrConfigSectionOwned = errors.New("config section registered by another owner")

// ConfigSectionRegistry holds registered sections by name, with the owner
// that registered each. A host embeds one to implement every role. The zero
// value is ready to use.
type ConfigSectionRegistry struct {
	mu       sync.RWMutex
	sections map[string]ConfigSection
	owners   map[string]string
}

// RegisterConfigSection installs s under name for the engine itself, the
// owner named by the empty string. A name a plugin holds is left alone.
func (r *ConfigSectionRegistry) RegisterConfigSection(name string, s ConfigSection) {
	_ = r.RegisterOwnedConfigSection("", name, s)
}

// RegisterOwnedConfigSection installs s under name for owner, replacing the
// owner's earlier section. A nil s removes the name. A name another owner
// holds is refused with ErrConfigSectionOwned, for a removal as well as a
// registration.
func (r *ConfigSectionRegistry) RegisterOwnedConfigSection(owner, name string, s ConfigSection) error {
	if name == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if held, ok := r.owners[name]; ok && held != owner {
		return fmt.Errorf("%w: %q", ErrConfigSectionOwned, name)
	}
	if s == nil {
		delete(r.sections, name)
		delete(r.owners, name)
		return nil
	}
	if r.sections == nil {
		r.sections = map[string]ConfigSection{}
		r.owners = map[string]string{}
	}
	r.sections[name] = s
	r.owners[name] = owner
	return nil
}

// ConfigSections returns the registered sections sorted by name, so a bundle
// lists its sections in the same order on every instance.
func (r *ConfigSectionRegistry) ConfigSections() []ConfigSection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.sections))
	for name := range r.sections {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]ConfigSection, 0, len(names))
	for _, name := range names {
		out = append(out, r.sections[name])
	}
	return out
}

var (
	_ ConfigSectionRegistrar      = (*ConfigSectionRegistry)(nil)
	_ ConfigSectionOwnerRegistrar = (*ConfigSectionRegistry)(nil)
	_ ConfigSectionProvider       = (*ConfigSectionRegistry)(nil)
)
