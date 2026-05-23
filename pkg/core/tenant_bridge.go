package core

import "context"

// TenantArchivedFunc checks whether a tenant slug is archived. Plugins
// implement this and the runtime wires it into the ReadOnlyArchived middleware.
type TenantArchivedFunc func(ctx context.Context, tenantSlug string) (archived bool, found bool)

// ArchivedCheckerProvider is an optional interface the plugin that owns the
// tenant registry implements to expose its tenant archival state. The runtime
// reads it after Start() from the one running plugin that implements it, and
// wires the ReadOnlyArchived middleware into the API router so mutating
// requests against archived tenants are rejected with HTTP 423 Locked.
type ArchivedCheckerProvider interface {
	Plugin

	// ArchivedChecker returns a TenantArchivedFunc that checks whether the
	// given tenant slug is in archived (read-only) state. The first return
	// value is the archived flag. The second is whether the tenant was found.
	ArchivedChecker() TenantArchivedFunc
}

// TenantValidatorFunc checks whether a tenant slug exists in the tenant
// registry (sys_tenants). Return true if the slug is known, false otherwise.
// Used by TenantHeader to validate X-Tenant-ID overrides from super_admin
// callers: prevents privilege escalation to non-existent tenant scopes.
type TenantValidatorFunc func(ctx context.Context, tenantSlug string) bool

// TenantValidatorProvider is an optional interface the plugin that owns the
// tenant registry implements to expose tenant existence validation. The
// runtime reads it after Start() from the one running plugin that implements
// it, and wires it into the TenantHeader middleware so super_admin
// X-Tenant-ID overrides are rejected when the target tenant does not exist.
type TenantValidatorProvider interface {
	Plugin

	// TenantValidator returns a TenantValidatorFunc that checks whether the
	// given tenant slug exists in sys_tenants.
	TenantValidator() TenantValidatorFunc
}

// TenantRosterFunc reports how many tenants the install has. It is allowed to
// stop counting at two: every caller only needs to tell "none", "exactly one"
// and "more than one" apart, and a full count over a table that has held
// hundreds of rows would be paid on every unresolved public request.
type TenantRosterFunc func(ctx context.Context) (count int, err error)

// TenantRosterProvider is an optional interface the plugin that owns the
// tenant registry implements to expose the size of the tenant roster. The
// runtime reads it after Start() from the one running plugin that implements
// it, and wires it into the public-route tenant guard, which needs to tell
// an install that has not been provisioned yet from one that has more than
// one tenant and was asked for a hostname belonging to none of them.
//
// Those two look identical at the point of the decision: both arrive with no
// tenant on the context. The first has to be served, because first-run setup
// answers before any tenant exists. The second is the case the 404 is for.
type TenantRosterProvider interface {
	Plugin

	// TenantRoster returns a TenantRosterFunc reporting the number of tenants
	// on the install, which may be capped at two.
	TenantRoster() TenantRosterFunc
}

// TenantSlugsFunc returns every tenant slug on the install, ordered. It is an
// unscoped read by design: the caller is enumerating tenants rather than
// reading their data, and every route behind it is super_admin only.
type TenantSlugsFunc func(ctx context.Context) ([]string, error)

// TenantSlugsProvider is an optional interface the plugin that owns the
// tenant registry implements to enumerate the roster. The runtime reads it
// after Start() from the one running plugin that implements it, and hands it
// to the cross-tenant DSAR export, which has to visit every tenant in turn to
// answer one subject's request.
//
// Without it the export refuses a cross-tenant request rather than answering a
// narrower one, because an export that silently covered fewer tenants than it
// claimed would be worse than no export.
type TenantSlugsProvider interface {
	Plugin

	// TenantSlugs returns every tenant slug on the install.
	TenantSlugs() TenantSlugsFunc
}

// DefaultTenantFunc registers the implicit default tenant. It is idempotent:
// an install that already has the row gets no second one.
type DefaultTenantFunc func(ctx context.Context) error

// DefaultTenantProvider is an optional interface the plugin that owns the
// tenant registry implements so first-run setup can give the bootstrap
// super_admin a tenant to belong to. The runtime reads it from the one
// running plugin that implements it.
//
// The engine cannot write that row itself. The tenant registry is the
// plugin's, down to which columns it has, and an engine that wrote it directly
// would carry a copy of the plugin's schema that nothing keeps in step.
//
// The default tenant is the one tenant a multi-tenant deployment has to have,
// and it is what unresolved traffic falls back to. The owning plugin
// registers it even where it refuses to create further tenants, so an install
// that turned multi-tenant mode on after setup can still register it through
// the API.
//
// Without it, first-run setup on a multi-tenant install leaves the account
// stamped with the tenant it names and no row on the roster. That is the same
// state an install bootstrapped by any other route reaches, and the login path
// already names it in a warning.
type DefaultTenantProvider interface {
	Plugin

	// DefaultTenant registers the implicit default tenant, idempotently.
	DefaultTenant() DefaultTenantFunc
}

// TenancyConnProvider is an optional interface the engine Host implements
// when tenant isolation is wired (multi-tenant mode). Plugins type-assert
// the host to this interface to acquire a tenant-scoped database connection
// before running background work or processing inbound webhook payloads.
// In single-tenant mode this interface is not implemented: type assertions
// silently fail and callers proceed without tenant isolation.
type TenancyConnProvider interface {
	// AcquireTenantConn returns a context whose Querier is bound to the
	// tenant's isolated schema/database. The returned cleanup func resets
	// session state and returns the connection to the pool. It MUST be
	// deferred by the caller. Returns (ctx, noop-cleanup, nil) when
	// tenantID is empty or tenancy is not configured.
	AcquireTenantConn(ctx context.Context, tenantID string) (context.Context, func(), error)
}
