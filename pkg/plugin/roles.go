package plugin

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// A role is a capability the engine wires from a running plugin. The engine
// finds it by the interface the plugin implements, never by the plugin's
// name, so any build can supply a role with a plugin of its own.

// Role describes one capability the engine wires from the running plugins.
type Role struct {
	// Name is the role's interface as Go prints it, such as
	// core.PIIMiddlewareProvider.
	Name string
	// Type is the interface a plugin implements to hold the role.
	Type reflect.Type
	// Single is true for a role exactly one plugin may hold: two running
	// plugins implementing it refuse the boot. Any number of running plugins
	// may implement a role that is not single.
	Single bool
}

func singleRole[T any]() Role { return newRole[T](true) }
func sharedRole[T any]() Role { return newRole[T](false) }

func newRole[T any](single bool) Role {
	t := reflect.TypeFor[T]()
	return Role{Name: t.String(), Type: t, Single: single}
}

// wiredRoles is every role the engine wires. CheckRoles reads the single ones
// at boot, and a build's wiring test holds the plugins it links to the list.
var wiredRoles = []Role{
	// The activator's accessors, which the runtime hands the routers.
	singleRole[security.MFAStoreProvider](),
	singleRole[security.DeviceRiskAssessorProvider](),
	singleRole[core.BruteForceMiddlewareProvider](),
	singleRole[core.CaptchaMiddlewareProvider](),
	singleRole[core.RequestSamplerProvider](),
	singleRole[core.IdempotencyMiddlewareProvider](),
	singleRole[observability.CaptureSinkProvider](),
	singleRole[core.CapturePolicyProvider](),
	singleRole[compliance.DSARAuditWriterProvider](),
	singleRole[core.APIKeyAuditLogProvider](),
	singleRole[core.APIKeyLookupProvider](),
	singleRole[core.APIKeyUpgradeProvider](),
	singleRole[core.PIIMiddlewareProvider](),
	singleRole[core.PIILogHandlerProvider](),

	// The roles the runtime reads itself: replication, tenancy and the cache
	// backends.
	singleRole[core.ClusterTransportProvider](),
	singleRole[core.ArchivedCheckerProvider](),
	singleRole[core.TenantValidatorProvider](),
	singleRole[core.TenantRosterProvider](),
	singleRole[core.TenantSlugsProvider](),
	singleRole[core.DefaultTenantProvider](),
	singleRole[core.MembershipProvider](),
	singleRole[core.AdminTokenStoreProvider](),
	singleRole[core.AuthBackendProvider](),
	singleRole[core.CacheBackendProvider](),

	// The roles the host hands to other plugins, asking the activator for the
	// holder on each call.
	singleRole[core.TenantRegionResolverProvider](),

	// Roles any number of running plugins contribute to.
	sharedRole[core.PreAuthMiddlewareProvider](),
	sharedRole[core.ChainMiddlewareProvider](),
	sharedRole[core.SecurityControlReporter](),
	sharedRole[core.AuthCacheConsumer](),
	sharedRole[compliance.SubjectEraserProvider](),
	sharedRole[compliance.SubjectExporterProvider](),
	sharedRole[core.CustomValidatorProvider](),
	sharedRole[compliance.HoldCheckerProvider](),
}

// WiredRoles returns every role the engine wires. A build's wiring test holds
// the plugins it links to the list: a role nothing implements is a feature
// that is off, and a single role two plugins implement is a boot that fails.
func WiredRoles() []Role {
	return append([]Role(nil), wiredRoles...)
}

// namedPlugin is a running plugin and the name it is registered under.
type namedPlugin struct {
	name   string
	plugin core.Plugin
}

// running returns the running plugins in name order. The map they are held in
// iterates randomly, and two plugins filling one role would otherwise be read
// in a different order on every boot, which shows up as one replica
// answering differently from the rest.
func (a *Activator) running() []namedPlugin {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]namedPlugin, 0, len(a.active))
	for name, ap := range a.active {
		if ap == nil || ap.plugin == nil || ap.phase != PhaseRunning {
			continue
		}
		out = append(out, namedPlugin{name: name, plugin: ap.plugin})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// Providers returns every running plugin that implements T, in plugin name
// order.
func Providers[T any](a *Activator) []T {
	running := a.running()
	out := make([]T, 0, len(running))
	for _, np := range running {
		if v, ok := np.plugin.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// Only returns the running plugin that implements T, for a role one plugin
// holds. ok is false when no running plugin implements it. Two or more are an
// error that names them, because which of them served the role would depend
// on nothing an operator can see.
func Only[T any](a *Activator) (T, bool, error) {
	var (
		holder  T
		holders []string
	)
	for _, np := range a.running() {
		v, ok := np.plugin.(T)
		if !ok {
			continue
		}
		if len(holders) == 0 {
			holder = v
		}
		holders = append(holders, np.name)
	}
	switch len(holders) {
	case 0:
		return holder, false, nil
	case 1:
		return holder, true, nil
	}
	var none T
	return none, false, roleClaimedTwice(reflect.TypeFor[T]().String(), holders)
}

// CheckRoles returns an error naming every single role that two or more
// running plugins implement. The runtime refuses to boot on it, because the
// engine can wire such a role from only one of them.
func (a *Activator) CheckRoles() error {
	running := a.running()
	var errs []error
	for _, r := range wiredRoles {
		if !r.Single {
			continue
		}
		var owners []string
		for _, np := range running {
			if reflect.TypeOf(np.plugin).Implements(r.Type) {
				owners = append(owners, np.name)
			}
		}
		if len(owners) > 1 {
			errs = append(errs, roleClaimedTwice(r.Name, owners))
		}
	}
	return errors.Join(errs...)
}

func roleClaimedTwice(role string, owners []string) error {
	return fmt.Errorf("%s is implemented by %d running plugins (%s), and exactly one may hold it",
		role, len(owners), strings.Join(owners, ", "))
}

// owner is Only for the accessors, which answer nil rather than an error. A
// role two running plugins implement has no owner here, and CheckRoles has
// already refused that boot.
func owner[T any](a *Activator) (T, bool) {
	p, ok, err := Only[T](a)
	if err != nil {
		a.logger.Error("plugin role left unwired", "err", err)
	}
	return p, ok
}
