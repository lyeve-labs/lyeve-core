package core

import (
	"context"
	"database/sql"
	"io/fs"
	"net/http"
	"sync"
)

// Bridge variables set by the plugin package at init time to avoid import
// cycles. Plugin repos (which are package plugin themselves) reference these
// as core.RegisterPlugin etc. to avoid naming collisions with their own
// package name.

// Registration is the one bridge that runs during init, so it cannot be a
// plain func var like the rest of this file.
//
// A plugin repo imports pkg/core and calls core.RegisterPlugin from its own
// init(). It does not import pkg/plugin, so no import edge orders the two
// init functions: whether pkg/plugin wins is decided by the whole binary's
// import graph, and adding one unrelated import elsewhere in the tree flips
// it. A plain func var would then be nil for every plugin whose init ran
// first, and the binary would die at boot with a nil dereference.
//
// Registrations that arrive before the registry is wired are buffered here
// and drained by SetPluginRegistrar. A buffer that is never drained means
// pkg/plugin was not linked into the binary at all, which cannot happen in
// anything that boots the engine: pkg/runtime imports it.
var (
	registrarMu    sync.Mutex
	registerFn     func(name string, factory PluginFactory)
	registerCapsFn func(name string, factory PluginFactory, caps Capability)
	pendingPlugins []pendingRegistration
)

type pendingRegistration struct {
	name    string
	factory PluginFactory
	caps    Capability
	hasCaps bool
}

// RegisterPlugin records a plugin factory under name. Plugin repos call this
// from init(), and the engine activates the plugin later, if entitled. The
// plugin declares no capabilities, so it runs with what the host capability
// policy grants its name, and with none when the policy has no entry for it.
func RegisterPlugin(name string, factory PluginFactory) {
	registrarMu.Lock()
	if registerFn == nil {
		pendingPlugins = append(pendingPlugins, pendingRegistration{name: name, factory: factory})
		registrarMu.Unlock()
		return
	}
	fn := registerFn
	registrarMu.Unlock()
	fn(name, factory)
}

// RegisterPluginWithCaps records a plugin factory with explicit capability
// restrictions, denying every host method outside the declared set.
func RegisterPluginWithCaps(name string, factory PluginFactory, caps Capability) {
	registrarMu.Lock()
	if registerCapsFn == nil {
		pendingPlugins = append(pendingPlugins,
			pendingRegistration{name: name, factory: factory, caps: caps, hasCaps: true})
		registrarMu.Unlock()
		return
	}
	fn := registerCapsFn
	registrarMu.Unlock()
	fn(name, factory, caps)
}

// SetPluginRegistrar wires the real registry and replays anything registered
// before it was available, in call order so duplicate detection still fires.
// Called once by plugin.init().
func SetPluginRegistrar(
	reg func(name string, factory PluginFactory),
	regCaps func(name string, factory PluginFactory, caps Capability),
) {
	registrarMu.Lock()
	registerFn, registerCapsFn = reg, regCaps
	pending := pendingPlugins
	pendingPlugins = nil
	registrarMu.Unlock()

	for _, p := range pending {
		if p.hasCaps {
			regCaps(p.name, p.factory, p.caps)
			continue
		}
		reg(p.name, p.factory)
	}
}

// AllPluginCaps returns the capability grant of every registered plugin, the
// set its scoped host enforces rather than the set it declared.
// Set by plugin.init().
var AllPluginCaps func() []PluginCapsEntry

// PluginCaps returns the registered capabilities for a plugin.
// Set by plugin.init().
var PluginCaps func(name string) Capability

// RegisteredPlugins returns the sorted list of registered plugin names.
// Set by plugin.init().
var RegisteredPlugins func() []string

// PluginMigrate runs plugin migrations against the migration DB.
// Set by plugin.init().
var PluginMigrate func(ctx context.Context, db *sql.DB, dialect string, migrationsFS fs.FS, tableName string) error

// PluginAppliedVersions returns the set of migration versions from the
// bookkeeping table. Set by plugin.init().
var PluginAppliedVersions func(ctx context.Context, db *sql.DB, tableName string) (map[string]bool, error)

// PluginMigrateRollback rolls back the last n plugin migrations.
// Set by plugin.init().
var PluginMigrateRollback func(ctx context.Context, db *sql.DB, dialect string, migrationsFS fs.FS, tableName string, n int) error

// LookupPlugin returns the registered factory for the named plugin, or nil.
// Set by plugin.init().
var LookupPlugin func(name string) PluginFactory

// ResetPluginRegistry clears the global plugin registry. Exposed for tests.
// Set by plugin.init().
var ResetPluginRegistry func()

// PluginCapsEntry is a snapshot of plugin name + caps. Caps is the grant the
// engine enforces. Explicit is false for a plugin that neither the host policy
// nor its own declaration scopes, and such a plugin's Caps is 0.
type PluginCapsEntry struct {
	Name     string
	Caps     Capability
	Explicit bool
}

// Route types (mirrored in core so plugin repos can reference them)

// RouteGroup categorizes a route for the kernel's middleware gating.
type RouteGroup string

const (
	GroupPublic     RouteGroup = "public"
	GroupAuth       RouteGroup = "auth"
	GroupAdmin      RouteGroup = "admin"
	GroupSuperAdmin RouteGroup = "super_admin"
)

// RouteDecl describes an HTTP route a plugin claims ownership of.
type RouteDecl struct {
	Method  string
	Pattern string
	Handler http.Handler
	Group   RouteGroup

	// MaxBodyBytes is the largest request body this route accepts. Zero means
	// the route takes the group's JSON limit, which is what almost every route
	// wants.
	//
	// A route that carries a file rather than a document has to say so: the
	// engine's body guards are mounted ahead of the handler, so a limit the
	// handler enforces on its own is only ever the smaller of the two, and a
	// plugin publishing a size it cannot receive is publishing a fiction. Only
	// a literal pattern may declare one: the outermost guard runs before
	// routing and matches paths, so a limit on a pattern holding {id} is
	// logged and ignored rather than half-applied.
	MaxBodyBytes int64

	// MediaTypes lists the request body media types this route accepts beyond
	// application/json and multipart/form-data, which every route accepts.
	// The engine refuses any other body with 415 before routing, so a route
	// spoken to by a protocol that fixes its own media type has to name it
	// here: a SCIM client sends application/scim+json, and a SAML identity
	// provider posts application/x-www-form-urlencoded.
	//
	// The admission holds only for a request the router serves under exactly
	// this method and pattern, so a declaration never reaches another route,
	// and it is dropped when an engine route shadows this one. It changes the
	// media type check and no other control: a session-authenticated route
	// still requires the CSRF token, so declaring a form media type there
	// does not open it to a cross-site form.
	MediaTypes []string

	// SelfScoping says this route carries the tenant it is for inside the
	// request itself, in something the engine issued and verifies, and so must
	// not be refused for arriving on a hostname that resolves no tenant.
	//
	// A public request names its tenant by the Host header and by nothing
	// else, because a caller that could name its own tenant on an
	// unauthenticated route could read any tenant's public surface by guessing
	// a slug, and a slug is not a secret. On an install with more than one
	// tenant, a public request whose Host resolves none is answered 404: that
	// host was never rostered, so nobody claimed it.
	//
	// That rule cannot be applied to every public route. A flow's inbound
	// webhook is the case in hand: the flow id in the path names its tenant,
	// the plugin resolves the flow from it, and the run happens in that
	// tenant. Refusing the request before the handler runs would reject a
	// delivery that reached the instance under any name but the rostered one,
	// and would buy no isolation, because the token decides the tenant and the
	// Host never did.
	//
	// So it is declared per route rather than inferred. A route qualifies only
	// when the tenant is inside something signed and the handler verifies it.
	// A route that merely reads a tenant out of a body or a query parameter is
	// the enumeration oracle this rule exists to close, and must not set it.
	SelfScoping bool

	// AdminGrant is the admin token grant that reaches this route, one of the
	// AdminGrant* constants. It is fail-closed: empty means session only, and
	// an admin token calling the route is refused whatever it holds. A name
	// outside the catalog, a grant on a route SessionOnlyAdminPatterns covers
	// or a declaration marks SessionOnly, or a grant on a GroupSuperAdmin
	// route stops the admin router from building. A token never carries
	// super_admin: its owner acts as an admin of the token's tenant.
	//
	// The grant only opens the door to a token. The route's group still
	// applies, checked against the token owner's current roles.
	AdminGrant string

	// Scope is the API key scope a GroupAuth route requires, as
	// resource:action, for a route the scope its path names does not fit.
	// Empty means the path's scope: the resource is the first segment after
	// the router's prefix and the action follows the method, as
	// BuildScopeRoute derives them. A route that declares nothing is
	// therefore never open to a key. The key must hold the scope the path
	// names, or a wildcard that covers it.
	//
	// Declare one where the method misstates what the route does, such as a
	// read that has to arrive as a POST. A key holding an admin role is held
	// to its role rather than its scopes, and a session carries no scopes,
	// so neither is affected. A value that is not resource:action refuses
	// every key short of the full wildcard, and the engine logs it at boot.
	// The field means nothing on a route of any other group.
	Scope string

	// RateLimit is the per-address limit on this route, and an operator's
	// PUBLIC_RATE_LIMITS entry for the route still overrides it. The engine
	// keeps limits only for the public routes it serves itself, so nil leaves
	// any other route on the global cap.
	//
	// The public limiter wraps GroupPublic routes only, so a limit on a route
	// of any other group is ignored.
	RateLimit *RouteRateLimit

	// Sensitive says the route's request or response body carries a
	// credential: a secret shown once, a password, or a code that confirms
	// one. Request capture then records the method, URL, headers and status
	// of a call and never its bodies. A capture sink redacts by field name,
	// and a field it does not know is stored where any tenant admin can
	// read it.
	Sensitive bool

	// SessionOnly says no admin token may ever reach this route, whoever owns
	// it. An empty AdminGrant already refuses every token. SessionOnly makes
	// that a rule the router enforces at build time: a grant on this route,
	// in this declaration or in any other, stops the admin router from
	// building. A route that mints a credential, changes an account or
	// destroys data across tenants declares it, because a token able to call
	// one could make itself permanent or do what no single tenant may. It
	// covers this route alone, so every route of a subtree that must stay
	// session only declares it.
	SessionOnly bool

	// Doc describes the route in the OpenAPI document. The document takes
	// the security, the roles and the license feature from the route's group
	// and owner either way, and Doc adds what only the route's author knows.
	// Nil documents the route from its method, pattern and group alone.
	Doc *RouteDoc
}

// RouteDoc is what the OpenAPI document says about one route. A field left
// empty keeps what the document would write without it: the owner's tag, a
// summary of the method and pattern, a string parameter for each segment of
// the pattern, and a 200 response.
type RouteDoc struct {
	Tag, Summary, Description string
	// Params lists the route's parameters in the order the document shows
	// them. A path segment of the pattern that Params does not name is still
	// documented, ahead of them.
	Params []RouteParam
	// RequestExample is a JSON body the route accepts, shown as the example
	// of a required request body. Nil documents no body.
	RequestExample any
	// Responses maps each status the route answers to its description.
	Responses map[int]string
}

// RouteParam is one documented parameter. In is "path", "query", "header"
// or "cookie", and a path parameter is always required. Type is its JSON
// Schema type, "string" when empty. Format and Default are written only when
// set.
type RouteParam struct {
	Name, In, Type, Format string
	Required               bool
	Default                any
}

// RouteRateLimit is a per-address token bucket: Rate requests a second
// sustained and Burst requests at once. Both must be positive. A zero rate
// never refills and a zero burst refuses the first request, so a declaration
// holding either is ignored.
type RouteRateLimit struct {
	Rate  float64
	Burst int
}

// RoutesPlugin is an optional interface a Plugin can implement to declare
// HTTP routes. The activator checks for this interface after Start() succeeds
// and collects the route declarations.
type RoutesPlugin interface {
	Plugin
	Routes() []RouteDecl
}

// PluginRoutes groups routes by plugin name.
type PluginRoutes struct {
	PluginName string
	Routes     []RouteDecl
}

// PluginPhase describes the lifecycle phase of a plugin.
type PluginPhase int

const (
	PhaseStarting PluginPhase = iota
	PhaseRunning
	PhaseStopping
	PhaseStopped
	PhaseFailed
)
