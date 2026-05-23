package core

import (
	"context"
	"strings"
)

// A rule is a role, a resource, the actions it grants and a field mask. The
// engine holds no rules. It declares the shape of one here and reads whatever
// checker a plugin registers, and it enforces RoleOnlyPermissionChecker when
// none does.
//
// A rule's resource is a schema name, "*" for every schema, or a resource of
// a kind a plugin registers with RegisterPermissionKind. A plugin's resources
// are governed through the same rules rather than a second set, so the plugin
// asks the registered checker whether a caller's roles allow an action on
// one, and the admin's Permissions page is where the rule is written. Every
// registered kind follows one set of rules, and a plugin resource is any
// resource of a registered kind.

const (
	// RoleSuperAdmin is the role every authorization gate exempts: it is the
	// one role that can write a permission rule, so a fresh install with no
	// rules has to admit it or nobody could write the first one.
	RoleSuperAdmin = "super_admin"

	// RoleAdmin is allowed every action on a resource of a kind registered
	// with AdminByDefault, until a rule names it for that resource.
	RoleAdmin = "admin"

	// PermissionResourceAll is the wildcard rule that applies to every
	// schema. It reaches schemas only, so a wildcard written for content never
	// widens into a grant on a plugin resource.
	PermissionResourceAll = "*"
)

// IsPluginPermissionResource reports whether resource belongs to a
// registered kind, as opposed to a schema or the wildcard. The checker treats
// every registered kind alike.
func IsPluginPermissionResource(resource string) bool {
	_, ok := permissionKindOf(resource)
	return ok
}

// GeneralPermissionResource returns the row consulted beside the resource's
// own: the wildcard for a schema, and the kind's General for a resource of a
// registered kind. For a General itself it is the resource, so a lookup
// lands on one row and never two.
func GeneralPermissionResource(resource string) string {
	if k, ok := permissionKindOf(resource); ok {
		return k.General
	}
	return PermissionResourceAll
}

// ValidatePermissionResource reports whether resource may name a rule: the
// wildcard, a resource of a registered kind, or a schema name. A kind's
// prefix with no slug, or with one the kind refuses, is refused rather than
// stored as a schema name nobody will ever match. Schema names are not
// checked against the schemas that exist, because a rule may be written
// before its schema.
func ValidatePermissionResource(resource string) bool {
	if resource == "" {
		return false
	}
	for _, k := range registeredPermissionKinds() {
		if slug, ok := strings.CutPrefix(resource, k.Prefix); ok {
			return k.ValidSlug(slug)
		}
	}
	return true
}

// PermissionActionsFor returns the actions a rule on resource may grant.
// Every resource takes create, read, update and delete. A resource of a
// registered kind also takes the actions its kind names.
func PermissionActionsFor(resource string) []string {
	actions := []string{"create", "read", "update", "delete"}
	if k, ok := permissionKindOf(resource); ok {
		actions = append(actions, k.Actions...)
	}
	return actions
}

// ValidPermissionAction reports whether action may appear in a rule on
// resource.
func ValidPermissionAction(resource, action string) bool {
	for _, a := range PermissionActionsFor(resource) {
		if a == action {
			return true
		}
	}
	return false
}

// PermissionChecker answers whether a caller's roles allow an action on a
// resource. The plugin that owns the rules implements it over the store they
// live in, and registers it through PermissionCheckerRegistrar, so the
// engine's content handlers and a plugin governing its own resources read one
// answer and a rule written on the Permissions page governs a flow the way it
// governs a schema.
//
// The rules, in order: super_admin is always allowed, as at every other gate,
// because only a super_admin can write a rule and a fresh install has none.
// A role's rule for the resource that names the action grants it. For one
// resource of a registered kind the rule for that resource is consulted
// first and the rule for the kind's General second, and the more specific
// one wins outright, so a rule for one resource with no actions denies it.
// On a resource of a kind registered with AdminByDefault the admin role is
// allowed everything while no rule names admin for it, so an install that
// has written no rule still lets its admins manage the plugin's resources.
// Any other role with no rule is refused. The wildcard "*" reaches schemas
// only.
//
// The error is a store failure and nothing else. A refusal is false with a
// nil error. A handler answers a store failure as 503 and a refusal as 403
// naming the action and resource, the shape the content handlers use.
type PermissionChecker interface {
	Allowed(ctx context.Context, roles []string, resource, action string) (bool, error)
}

// PermissionResourceRules is implemented by a registered PermissionChecker
// that keeps a rule store, so a plugin reaches it by asserting the checker it
// already reads. RoleOnlyPermissionChecker keeps no rules and does not
// implement it. A rule on a plugin resource belongs to the tenant it was
// written in, and the tenant is read from ctx.
//
// MoveResourceRules is for a rename: the tenant's rules on from move to to,
// and a rule every tenant shares on from is copied to to as the tenant's own
// where the tenant has none for that role, so the renamed resource keeps the
// access it had. DropResourceRules is for a delete: the tenant's rules on the
// resource go, so a later resource given the same slug starts clean. Both
// take one resource of a registered kind, never the kind's General, and
// leave other tenants' rules alone.
type PermissionResourceRules interface {
	MoveResourceRules(ctx context.Context, from, to string) error
	DropResourceRules(ctx context.Context, resource string) error
}

// PermissionCheckerRegistrar is implemented by the engine host and forwarded
// by ScopedHost. The plugin that owns the rules calls it when it starts with
// its checker, and again with nil when it stops, so authorization follows the
// plugin's lifecycle rather than the process.
//
// Registering nil does not leave the engine without an answer. The host falls
// back to RoleOnlyPermissionChecker, which answers as an install with no rules
// does.
type PermissionCheckerRegistrar interface {
	RegisterPermissionChecker(c PermissionChecker)
}

// PermissionCheckerProvider is implemented by the engine host and forwarded
// by ScopedHost. The engine's content handlers and any plugin that governs a
// resource read PermissionChecker() per request, never at Start.
//
// It never returns nil. With no rule engine registered it returns
// RoleOnlyPermissionChecker, so a caller has one answer to read and no nil
// branch that could be mistaken for a grant.
type PermissionCheckerProvider interface {
	PermissionChecker() PermissionChecker
}

// FieldMasker reports the fields a caller may not read. It is optional on a
// PermissionChecker: a checker with no field rules does not implement it, and
// a reader type-asserts for it rather than requiring it.
//
// The engine's content reads call it for every entry they serve. An empty
// result hides nothing. The error is a store failure and nothing else, and a
// read that cannot resolve its mask is answered 503 rather than served with
// fields the caller may not be allowed to see.
type FieldMasker interface {
	FieldMask(ctx context.Context, roles []string, resource string) ([]string, error)
}

// RoleOnlyPermissionChecker is the engine's authorization when no plugin has
// registered a rule engine. The kernel ships roles on sys_users and no rule
// store, so this is the ordinary configuration of a build with no rule engine
// registered rather than an error path.
//
// It is the empty rule set, and it answers exactly what a rule store holding
// no rows answers, which is what every install does before its first rule is
// written:
//
//   - super_admin is allowed everything, because only a super_admin can write
//     a rule and a fresh install has none. Refusing it would leave the one
//     role able to grant access as the one being refused.
//   - admin is allowed a resource of a kind registered with AdminByDefault
//     while no rule names admin for it, so an install with no rules still
//     lets its admins manage them.
//   - every other role is refused every action on every resource.
//
// It is deliberately neither of the two easy answers. Allowing everything
// would make a build with no rule engine an open database, and denying
// everything would leave no caller able to write the first rule. It masks no
// field, because a mask only ever comes from a rule, so it does not implement
// FieldMasker.
//
// A build in this state says so once at boot. An operator who wants rules
// installs the plugin that owns them.
type RoleOnlyPermissionChecker struct{}

var _ PermissionChecker = RoleOnlyPermissionChecker{}

// Allowed implements PermissionChecker. It reads no table, so it never fails.
func (RoleOnlyPermissionChecker) Allowed(_ context.Context, roles []string, resource, _ string) (bool, error) {
	kind, ok := permissionKindOf(resource)
	adminByDefault := ok && kind.AdminByDefault
	for _, role := range roles {
		switch role {
		case RoleSuperAdmin:
			return true, nil
		case RoleAdmin:
			if adminByDefault {
				return true, nil
			}
		}
	}
	return false, nil
}
