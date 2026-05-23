package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// PermissionKind is a kind of resource a plugin governs through the permission
// rules, beside the schemas the engine governs itself. A rule names every
// resource of the kind by General, or one of them as Prefix followed by its
// slug, and the checker reads that rule the way it reads a rule on a schema.
//
// A plugin registers its kind from init, so the kind is known before any rule
// is validated and before any checker answers.
type PermissionKind struct {
	// General names every resource of the kind, as "reports".
	General string

	// Prefix names one resource of the kind when a slug follows it, as
	// "report:" does in "report:weekly". It ends with a colon, which no
	// schema name can carry, so a resource of the kind never reads as a
	// schema.
	Prefix string

	// ValidSlug reports whether a slug can name a resource of the kind. A
	// rule naming a slug it refuses is refused rather than stored, because
	// it could never match anything.
	ValidSlug func(slug string) bool

	// Actions are the actions a rule on the kind may grant beyond create,
	// read, update and delete, which every resource takes.
	Actions []string

	// AdminByDefault allows the admin role every action on a resource of the
	// kind while no rule names admin for it, so an install that has written
	// no rule still lets its admins manage the plugin's resources.
	AdminByDefault bool
}

// permissionKinds holds the registered kinds, sorted by General. A
// registration replaces the slice rather than editing it, so a reader may
// keep the slice it was handed after the lock is released.
//
// The kernel registers no kind of its own. Every kind arrives from the
// plugin that owns its resources, so a build that links none of them reads
// every rule as a rule on a schema.
var permissionKinds = struct {
	mu    sync.RWMutex
	kinds []PermissionKind
}{}

// RegisterPermissionKind adds a kind of resource the permission rules can
// name. A kind whose General is already registered replaces the registered
// one, so a plugin that registers its kind twice changes nothing.
//
// It panics on a kind that cannot work: an empty General, the wildcard, a
// General holding a colon, a Prefix that does not end with a colon, no slug
// rule, an empty action, or a Prefix that overlaps the Prefix of another
// kind. Each is a fault in the calling plugin. Ignoring the kind instead
// would leave its resources read as schema names, which changes who may act
// on them without a word in the log.
func RegisterPermissionKind(k PermissionKind) {
	if err := k.validate(); err != nil {
		panic("core.RegisterPermissionKind: " + err.Error())
	}
	k.Actions = slices.Clone(k.Actions)

	permissionKinds.mu.Lock()
	defer permissionKinds.mu.Unlock()

	next := make([]PermissionKind, 0, len(permissionKinds.kinds)+1)
	for _, have := range permissionKinds.kinds {
		if have.General == k.General {
			continue
		}
		if strings.HasPrefix(have.Prefix, k.Prefix) || strings.HasPrefix(k.Prefix, have.Prefix) {
			panic(fmt.Sprintf("core.RegisterPermissionKind: prefix %q of %q overlaps prefix %q of %q",
				k.Prefix, k.General, have.Prefix, have.General))
		}
		next = append(next, have)
	}
	next = append(next, k)
	slices.SortFunc(next, func(a, b PermissionKind) int { return strings.Compare(a.General, b.General) })
	permissionKinds.kinds = next
}

// PermissionKinds returns the registered kinds, sorted by General. Each
// Actions slice is a copy, so a caller cannot edit the registry through it.
func PermissionKinds() []PermissionKind {
	kinds := registeredPermissionKinds()
	out := make([]PermissionKind, len(kinds))
	for i, k := range kinds {
		k.Actions = slices.Clone(k.Actions)
		out[i] = k
	}
	return out
}

// registeredPermissionKinds returns the registry's own slice. The caller
// reads it and never writes to it.
func registeredPermissionKinds() []PermissionKind {
	permissionKinds.mu.RLock()
	defer permissionKinds.mu.RUnlock()
	return permissionKinds.kinds
}

// permissionKindOf returns the kind resource belongs to: the kind it names by
// General, or the kind whose Prefix it carries in front of a slug that kind
// accepts. Prefixes never overlap, so at most one kind matches.
func permissionKindOf(resource string) (PermissionKind, bool) {
	for _, k := range registeredPermissionKinds() {
		if resource == k.General {
			return k, true
		}
		if slug, ok := strings.CutPrefix(resource, k.Prefix); ok && k.ValidSlug(slug) {
			return k, true
		}
	}
	return PermissionKind{}, false
}

func (k PermissionKind) validate() error {
	switch {
	case k.General == "":
		return errors.New("general resource name is empty")
	case k.General == PermissionResourceAll:
		return fmt.Errorf("general resource name %q is the schema wildcard", k.General)
	case strings.Contains(k.General, ":"):
		return fmt.Errorf("general resource name %q holds a colon", k.General)
	case len(k.Prefix) < 2 || !strings.HasSuffix(k.Prefix, ":"):
		return fmt.Errorf("prefix %q of %q must be a name ending with a colon", k.Prefix, k.General)
	case k.ValidSlug == nil:
		return fmt.Errorf("kind %q has no slug rule", k.General)
	}
	if slices.Contains(k.Actions, "") {
		return fmt.Errorf("kind %q names an empty action", k.General)
	}
	return nil
}
