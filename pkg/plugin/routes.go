package plugin

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// RouteGroup categorizes a route for the kernel's middleware gating.
type RouteGroup = core.RouteGroup

const (
	GroupPublic     = core.GroupPublic
	GroupAuth       = core.GroupAuth
	GroupAdmin      = core.GroupAdmin
	GroupSuperAdmin = core.GroupSuperAdmin
)

// RouteDecl describes an HTTP route a plugin claims ownership of.
type RouteDecl = core.RouteDecl

// RoutesPlugin is an optional interface a Plugin can implement to declare
// HTTP routes. The activator checks for this interface after Start()
// succeeds and collects the route declarations. The kernel then mounts
// them on the engine's HTTP mux.
//
// If two plugins claim the same (method, pattern) pair, activation fails
// with a conflict error: the kernel enforces route ownership.
type RoutesPlugin = core.RoutesPlugin
