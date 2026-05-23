package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/metrics"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/tracing"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

// RouterOption is a functional option for NewAdminRouter / NewAPIRouter.
type RouterOption func(*routerOptions)

type routerOptions struct {
	extra                []func(http.Handler) http.Handler
	preAuth              []func(http.Handler) http.Handler      // runs after CORS, before JWT auth
	pluginStatus         PluginStatusProvider                   // nil -> empty status report
	securityControls     SecurityControlsProvider               // nil -> 404, no runtime behind the router
	pluginSchema         PluginSchemaProvider                   // nil -> plugin schemas not served
	pluginReloader       PluginConfigReloader                   // nil -> saved config applies on restart only
	entitlements         EntitlementProvider                    // nil -> unlicensed behavior
	licenseModule        bool                                   // false -> the entitlements say no licensing implementation is linked
	pluginRoutes         []plugin.PluginRoutes                  // routes collected from the activator
	ownerRoutes          []plugin.PluginRoutes                  // routes whose owner is not a plugin, by owner
	licenseOwners        map[string]bool                        // owners whose routes the document describes in the engine's own words
	apiKeyAuth           func(http.Handler) http.Handler        // nil -> no API key auth
	apiKeyAudit          func(http.Handler) http.Handler        // nil -> no API key audit
	mfaStore             security.MFAStore                      // nil -> no MFA
	refreshTokenStore    *auth.RefreshTokenStore                // nil -> refresh tokens disabled
	lockoutBackend       core.CacheBackend                      // nil -> login and MFA lockouts are per process
	deviceRiskAssessor   security.DeviceRiskAssessor            // nil -> no device fingerprinting
	poolHealthProvider   observability.PoolHealthProvider       // nil -> pool health not configured
	archivedChecker      core.TenantArchivedFunc                // nil -> no archived-tenant guard
	tenantValidator      core.TenantValidatorFunc               // nil -> no X-Tenant-ID validation
	tenantRoster         core.TenantRosterFunc                  // nil -> no public-route tenant guard
	defaultTenant        core.DefaultTenantFunc                 // nil -> first-run setup registers no tenant
	memberships          core.MembershipReader                  // nil -> every session acts in its home tenant only
	adminTokens          core.AdminTokenStore                   // nil -> every admin token is refused and no token route is served
	requestCapture       observability.CaptureSink              // nil -> no request capture
	capturePolicy        core.CapturePolicy                     // nil -> every request captured for core.DefaultCaptureTTL
	dsarAuditWriter      compliance.DSARAuditWriter             // nil -> no DSAR audit logging
	dsarTenants          compliance.TenantLister                // nil -> a cross-tenant DSAR export is refused
	dsarTenantScope      compliance.TenantScopeFunc             // nil -> tenants are scoped by context value only
	healthProbes         *ProbeRegistry                         // nil -> probes not configured
	latencyTracker       *apimw.LatencyTracker                  // nil -> latency tracking disabled
	scalingHost          core.Host                              // the engine host: goroutine tracker, and the audit writer's database
	setupToken           *SetupToken                            // nil -> built from cfg.SetupToken
	keyStore             *encryption.KeyStore                   // nil -> build from cfg.EncryptionKey
	idempotency          func(http.Handler) http.Handler        // nil -> no Idempotency-Key enforcement, filled through core.IdempotencyMiddlewareProvider
	customRoutes         core.CustomRouteProvider               // nil -> an unrouted path is a 404, API router only
	endpointDocs         core.EndpointDocumenter                // nil -> the OpenAPI document lists declared routes only
	publicRateLimits     map[string]apimw.PublicRateLimitConfig // method:pattern -> rate config, nil -> defaults
	publicGlobalLimit    *apimw.PublicRateLimitConfig           // optional aggregate per-IP cap
	publicLimitOverrides map[string]apimw.PublicRateLimitConfig // operator overrides merged over the table in effect
	publicGlobalOverride *apimw.PublicRateLimitConfig           // operator override for the aggregate cap
	trustedProxies       apimw.TrustedProxies                   // CIDRs whose XFF is trusted (nil -> RemoteAddr only)
	contentStoreSink     func(*db.ContentStore)                 // nil -> the router's content store is not shared
	permissionChecker    core.PermissionCheckerProvider         // nil -> the kernel's role-only checker
	schemaSource         core.SchemaSource                      // nil -> this install has no schema engine
	contentLocalizer     core.ContentLocalizerProvider          // nil -> content reads ignore any locale
	recordRevisions      core.RecordRevisionStoreProvider       // nil -> the revision routes answer 503
	lifetime             context.Context                        //nolint:containedctx // nil -> context.Background(): how long this router's background work runs
}

// lifetimeCtx answers how long this router's background work runs. A caller
// that builds a router for the life of the process leaves it unset.
func (o *routerOptions) lifetimeCtx() context.Context {
	if o.lifetime != nil {
		return o.lifetime
	}
	return context.Background()
}

// WithLifetime bounds the background work the router starts: the admin token
// request log and the device sign-in pruner. Both run until the context ends,
// so a caller that replaces a router, as the hot reload does on every plugin
// route change, cancels the old one's context and the work it left behind
// stops with it. Unset, the work runs for the life of the process.
func WithLifetime(ctx context.Context) RouterOption {
	return func(o *routerOptions) {
		o.lifetime = ctx
	}
}

// WithSetupToken hands the admin router the process's setup token. The
// runtime builds it once, so a router rebuilt by hot reload keeps the token
// the log already printed and the retirement the first claim recorded.
func WithSetupToken(t *SetupToken) RouterOption {
	return func(o *routerOptions) {
		o.setupToken = t
	}
}

// WithMiddleware appends one or more middleware to the router chain.
// Applied after the built-in middleware stack (rate limiter, response time, etc.).
func WithMiddleware(mw ...func(http.Handler) http.Handler) RouterOption {
	return func(o *routerOptions) {
		o.extra = append(o.extra, mw...)
	}
}

// WithPreAuthMiddleware appends middleware that runs after CORS but before
// JWT authentication. Both routers mount the slot in the order given, and
// both mount it above auth and tenancy:
//
//	CORS -> pre-auth -> JWT auth -> TenantHeader -> extra -> handler
//
// The runtime fills it with request logging first, then whatever each running
// plugin returns from core.PreAuthMiddlewareProvider. That is above tenancy
// because a plugin resolving a tenant from the request itself, by mapping a
// custom domain in the Host header onto a slug, has to put the answer on the
// context with core.WithTenantID before TenantHeader looks for it:
// TenantHeader consults the context only after its header and claim branches
// come up empty, so anything mounted lower is too late to be read.
func WithPreAuthMiddleware(mw ...func(http.Handler) http.Handler) RouterOption {
	return func(o *routerOptions) {
		o.preAuth = append(o.preAuth, mw...)
	}
}

// WithPluginRoutes injects route declarations collected from the plugin
// activator. The kernel mounts these on the router with the appropriate
// middleware gating determined by RouteDecl.Group.
func WithPluginRoutes(routes []plugin.PluginRoutes) RouterOption {
	return func(o *routerOptions) { o.pluginRoutes = routes }
}

// WithOwnerRoutes mounts routes whose owner is not a plugin, such as the
// licensing implementation a build links. They are mounted, guarded,
// documented and read for their declared policy exactly as a plugin's are,
// with the differences that follow from there being no plugin: no tenant can
// have them withheld, the metrics name no plugin for them, and the OpenAPI
// document names no license feature for them and tags them with the owner's
// name unless their Doc says otherwise. Each call adds one owner's routes.
func WithOwnerRoutes(owner string, routes []core.RouteDecl) RouterOption {
	return func(o *routerOptions) {
		o.ownerRoutes = append(o.ownerRoutes, plugin.PluginRoutes{Name: owner, Routes: routes})
	}
}

// WithLicenseRoutes mounts the routes the licensing implementation serves,
// such as the license renewal and the tenant features pair. They are an
// owner's routes in every respect but one: the OpenAPI document describes
// them in the engine's own words, so it reads the same whichever
// implementation serves them.
func WithLicenseRoutes(owner string, routes []core.RouteDecl) RouterOption {
	return func(o *routerOptions) {
		o.ownerRoutes = append(o.ownerRoutes, plugin.PluginRoutes{Name: owner, Routes: routes})
		if o.licenseOwners == nil {
			o.licenseOwners = map[string]bool{}
		}
		o.licenseOwners[owner] = true
	}
}

// WithPluginStatus injects the runtime's plugin activator (or any matching
// provider) so /api/admin/plugins/status returns live activation state. When
// not set, the endpoint returns an empty report: useful for a build with no
// activator wired.
func WithPluginStatus(p PluginStatusProvider) RouterOption {
	return func(o *routerOptions) { o.pluginStatus = p }
}

// WithSecurityControls injects the runtime's control liveness report so
// /api/admin/security/controls answers with what is enforcing right now.
// When not set, the endpoint answers 404: a router with no runtime behind
// it has no controls to report on.
func WithSecurityControls(p SecurityControlsProvider) RouterOption {
	return func(o *routerOptions) { o.securityControls = p }
}

// WithPluginSchema injects the plugin schema provider (typically the
// activator) so /api/admin/plugins/{name}/schema returns JSON Schema for
// auto-generating configuration forms. When not set, the endpoint returns
// 404 for all names.
func WithPluginSchema(p PluginSchemaProvider) RouterOption {
	return func(o *routerOptions) { o.pluginSchema = p }
}

// WithSchemaSource supplies the registry the content path reads definitions
// from. Without it the router reads the engine's own table, which is the
// built-in path. With it an install serves content from a schema engine this
// module does not ship.
func WithSchemaSource(s core.SchemaSource) RouterOption {
	return func(o *routerOptions) { o.schemaSource = s }
}

// WithPluginConfigReloader injects the provider (typically the activator) that
// asks plugins to rebuild configuration-derived state after an operator saves a
// change. When not set, a save still applies to every plugin that reads its
// configuration per request. One that cached it at Start picks it up on the
// next restart.
func WithPluginConfigReloader(p PluginConfigReloader) RouterOption {
	return func(o *routerOptions) { o.pluginReloader = p }
}

// WithMFAStore injects the MFA secret store used by MFAVerify.
// When not set, the MFA verification endpoint returns 501 Not Implemented.
func WithMFAStore(s security.MFAStore) RouterOption {
	return func(o *routerOptions) { o.mfaStore = s }
}

// WithRefreshTokenStore injects the refresh token store for Refresh/Logout
// handlers. When not set, refresh token endpoints return 501 Not Implemented.
func WithRefreshTokenStore(store *auth.RefreshTokenStore) RouterOption {
	return func(o *routerOptions) { o.refreshTokenStore = store }
}

// WithLockoutBackend moves the login and MFA lockouts of every auth handler
// the router builds into backend. Pass it only a backend every replica
// shares: the point is one attempt limit for the deployment, one that a
// restart does not lift, and a used MFA challenge refused on every replica.
// When not set, each process counts its own attempts.
func WithLockoutBackend(backend core.CacheBackend) RouterOption {
	return func(o *routerOptions) { o.lockoutBackend = backend }
}

// WithAPIKeyAuth injects the X-API-Key authentication middleware.
// The middleware hashes the presented key and looks it up in sys_api_keys.
// When not set, X-API-Key requests fall through to JWT authentication.
func WithAPIKeyAuth(mw func(http.Handler) http.Handler) RouterOption {
	return func(o *routerOptions) { o.apiKeyAuth = mw }
}

// WithAPIKeyAudit injects per-request API key audit logging middleware.
// The middleware records method, path, status, IP, and timestamp for every
// X-API-Key-authenticated request. JWT requests are not audited.
func WithAPIKeyAudit(mw func(http.Handler) http.Handler) RouterOption {
	return func(o *routerOptions) { o.apiKeyAudit = mw }
}

// WithDeviceRiskAssessor injects the device risk assessor for the auth handler.
// Passed from the runtime: the assessor a plugin supplies.
func WithDeviceRiskAssessor(ra security.DeviceRiskAssessor) RouterOption {
	return func(o *routerOptions) { o.deviceRiskAssessor = ra }
}

// WithIdempotencyMiddleware fills the idempotency slot: the middleware a
// plugin contributes through core.IdempotencyMiddlewareProvider,
// mounted after every other chain-level middleware and before the routes on
// both routers, so a key spent on one is known to the other. Nil mounts
// nothing.
func WithIdempotencyMiddleware(m func(http.Handler) http.Handler) RouterOption {
	return func(o *routerOptions) { o.idempotency = m }
}

// WithCustomRoutes fills the custom route slot on the API router: a request
// no registered route matched is offered to the provider before the 404, the
// position core.CustomRouteProvider documents. Nil leaves every unrouted path
// a 404.
func WithCustomRoutes(p core.CustomRouteProvider) RouterOption {
	return func(o *routerOptions) { o.customRoutes = p }
}

// WithEndpointDocumenter adds the caller's tenant's plugin endpoints to the
// OpenAPI document, beside the declared routes.
func WithEndpointDocumenter(d core.EndpointDocumenter) RouterOption {
	return func(o *routerOptions) { o.endpointDocs = d }
}

// WithTrustedProxies sets the CIDR ranges whose X-Forwarded-For / X-Real-IP
// headers are trusted for client IP extraction.  When the immediate peer
// (RemoteAddr) is in one of these ranges, the rightmost untrusted XFF entry
// is used as the client IP.  Otherwise RemoteAddr is used directly (safe
// default, spoof-proof but proxy-blind).
func WithTrustedProxies(tp apimw.TrustedProxies) RouterOption {
	return func(o *routerOptions) { o.trustedProxies = tp }
}

// WithPublicRateLimitOverrides applies an operator's PUBLIC_RATE_LIMITS and
// PUBLIC_RATE_LIMIT_GLOBAL settings, parsed by
// apimw.ParsePublicRateLimits. The per-route entries merge over the table
// already in effect and a nil global leaves the aggregate cap alone, so a
// deployment that names one route keeps every other limit it had.
func WithPublicRateLimitOverrides(configs map[string]apimw.PublicRateLimitConfig, global *apimw.PublicRateLimitConfig) RouterOption {
	return func(o *routerOptions) {
		o.publicLimitOverrides = configs
		o.publicGlobalOverride = global
	}
}

// WithKeyStore injects a pre-built data-at-rest encryption KeyStore (master
// KEK -> per-tenant DEK). Passed from the runtime, which resolves the KEK
// through the secret-custody chain (a KMS/Vault plugin source in front of the
// process environment) so the master key need not live in ENCRYPTION_KEY.
// When not set, the router falls back to building a KeyStore from
// cfg.EncryptionKey: preserving the existing env-only behavior.
func WithKeyStore(ks *encryption.KeyStore) RouterOption {
	return func(o *routerOptions) { o.keyStore = ks }
}

// WithPoolHealthProvider injects the pool health provider so the
// /api/admin/pool/health endpoint returns live connection pool statistics.
// When not set, the endpoint returns 200 with a "not configured" message.
func WithPoolHealthProvider(p observability.PoolHealthProvider) RouterOption {
	return func(o *routerOptions) { o.poolHealthProvider = p }
}

// WithArchivedChecker injects the tenant archived-state check function.
// Passed from the runtime when a plugin supplies it. When set, the API
// router installs the ReadOnlyArchived middleware that rejects mutating
// requests (POST/PUT/PATCH/DELETE) against archived tenants with
// HTTP 423 Locked.
func WithArchivedChecker(fn core.TenantArchivedFunc) RouterOption {
	return func(o *routerOptions) {
		o.archivedChecker = fn
	}
}

// WithTenantValidator injects the tenant existence validator function.
// Passed from the runtime when a plugin keeps a tenant roster. When set, the
// API router passes it to TenantHeader so super_admin X-Tenant-ID overrides
// are validated against that roster. Non-existent tenants return 404.
func WithTenantValidator(fn core.TenantValidatorFunc) RouterOption {
	return func(o *routerOptions) {
		o.tenantValidator = fn
	}
}

// WithDefaultTenant injects the registration of the implicit default tenant.
// Passed from the runtime when a plugin keeps a tenant roster. When set,
// first-run setup on a multi-tenant install registers the default tenant so
// the bootstrap super admin belongs somewhere. Unset, setup creates the
// account and leaves the roster alone, and the login path warns about the
// account.
func WithDefaultTenant(fn core.DefaultTenantFunc) RouterOption {
	return func(o *routerOptions) {
		o.defaultTenant = fn
	}
}

// WithMembershipStore injects the cross-tenant membership reader a plugin
// supplied. When set, a sign-in may ask to act in a tenant other than the
// account's home one, and the roles for the session come from that membership.
//
// Without it every session acts at home with the roles on the account row, and
// a sign-in naming any other tenant is refused with what a wrong password is
// refused with. That is the single-tenant install and it is the ordinary
// shape, not a degraded one. Nothing is granted that the account row does not
// already grant, and nobody loses the tenant they belong to.
func WithMembershipStore(m core.MembershipReader) RouterOption {
	return func(o *routerOptions) {
		o.memberships = m
	}
}

// WithAdminTokenStore injects the admin token store a plugin supplied. When
// set, the admin router authenticates admin tokens and serves the routes that
// issue, rotate, revoke and audit them.
//
// Without it the router refuses every admin token presented and mounts none of
// those routes. A credential store that cannot answer has to be read as
// answering no, so the refusal is the whole behavior rather than a fallback to
// anonymous. Session sign-in is untouched.
func WithAdminTokenStore(s core.AdminTokenStore) RouterOption {
	return func(o *routerOptions) {
		o.adminTokens = s
	}
}

// WithTenantRoster injects the tenant roster size function. Passed from the
// runtime when a plugin keeps a tenant roster. When set, a public plugin route
// that has not declared itself self-scoping answers 404 to a request that
// resolved no tenant on an install holding more than one, because such a
// request arrived on a hostname nobody rostered and there is no honest tenant
// to serve it as.
//
// Without it the guard is not mounted at all, which is the single-tenant
// engine and any install without a roster.
func WithTenantRoster(fn core.TenantRosterFunc) RouterOption {
	return func(o *routerOptions) {
		o.tenantRoster = fn
	}
}

// WithRequestCapture injects a CaptureSink for request/response debugging.
// Passed from the runtime when a plugin supplies a capture sink. When set,
// every request flowing through the router is captured and handed to the sink
// for persistence. A nil sink disables capture (no-op).
func WithRequestCapture(sink observability.CaptureSink) RouterOption {
	return func(o *routerOptions) { o.requestCapture = sink }
}

// WithCapturePolicy injects the policy that decides which requests the
// capture sink receives and how long each is kept. Passed from the runtime
// when a plugin supplies one. A nil policy captures every request for
// core.DefaultCaptureTTL.
func WithCapturePolicy(policy core.CapturePolicy) RouterOption {
	return func(o *routerOptions) { o.capturePolicy = policy }
}

// WithDSARAudit injects the DSARAuditWriter for GDPR DSAR audit logging.
// Passed from the runtime when a plugin supplies a DSAR audit writer. When
// not set, DSAR operations are NOT audit-logged, and the handler says so in
// the log.
func WithDSARAudit(w compliance.DSARAuditWriter) RouterOption {
	return func(o *routerOptions) { o.dsarAuditWriter = w }
}

// WithDSARTenants injects the tenant enumeration and per-tenant scoping a
// cross-tenant DSAR export needs. Passed from the runtime, which is where the
// host lives.
//
// Both are required for a sweep and both are optional here. When either is
// absent the endpoint refuses a cross-tenant request rather than falling back
// to a single tenant, because a bundle that silently covers one tenant while
// the caller asked for all of them is wrong.
func WithDSARTenants(list compliance.TenantLister, scope compliance.TenantScopeFunc) RouterOption {
	return func(o *routerOptions) {
		o.dsarTenants = list
		o.dsarTenantScope = scope
	}
}

// WithLatencyTracker injects a LatencyTracker so the /api/admin/debug/latency
// endpoint returns live per-endpoint p50/p95/p99 statistics.
// Passed from the runtime after the tracker is initialized.
func WithLatencyTracker(lt *apimw.LatencyTracker) RouterOption {
	return func(o *routerOptions) { o.latencyTracker = lt }
}

// WithScalingHost injects the engine host. The name comes from the scaling
// endpoints (GC configuration, goroutine tracker), but it is the engine host
// itself: the schema handler writes its audit entries through the same
// one, and without it schema DDL is applied with no record of who applied it.
// When not set, the goroutine endpoint returns only runtime.NumGoroutine().
func WithScalingHost(host core.Host) RouterOption {
	return func(o *routerOptions) { o.scalingHost = host }
}

// WithContentStoreSink hands the caller the cached content store this router
// serves reads from, so the plugin host can write through the same instance.
// Without it a plugin write lands in the database but never invalidates the
// read path's cached lists.
func WithContentStoreSink(sink func(*db.ContentStore)) RouterOption {
	return func(o *routerOptions) { o.contentStoreSink = sink }
}

// WithPermissionChecker injects the host the content reads ask for
// authorization. It is read per request, not at build time, so the checker
// the plugin that owns the rules registers in Start is picked up, and the one
// it clears in Stop stops being used without rebuilding the router.
//
// Absent the option, and on a build with no such plugin, the kernel's
// core.RoleOnlyPermissionChecker answers instead: super_admin is allowed
// everything, admin is allowed a resource of a kind registered with
// AdminByDefault, and every other role is refused. That is what an install with no rules
// written enforces.
func WithPermissionChecker(p core.PermissionCheckerProvider) RouterOption {
	return func(o *routerOptions) { o.permissionChecker = p }
}

// WithContentLocalizer injects the provider the content handlers ask, per
// request, for the localizer a plugin registered. The provider is the engine
// host, asked each time rather than once, so the registration follows the
// plugin's lifecycle: a locale in a request is honored while a localizer is
// registered and ignored otherwise.
func WithContentLocalizer(p core.ContentLocalizerProvider) RouterOption {
	return func(o *routerOptions) { o.contentLocalizer = p }
}

// WithRecordRevisionStore injects the provider the content handlers ask, per
// request, for the store that keeps revision snapshots. The plugin that owns
// the rows registers one in Start and clears it in Stop, so the routes follow
// that lifecycle without rebuilding the router.
//
// Without it the two revision routes answer 503 and an update records no
// history. They never answer an empty list, which would say the record has
// never been edited.
func WithRecordRevisionStore(p core.RecordRevisionStoreProvider) RouterOption {
	return func(o *routerOptions) { o.recordRevisions = p }
}

// resolveKeyStore returns the data-at-rest KeyStore for the auth handler.
// An injected KeyStore (o.keyStore, from the runtime's secret-custody chain)
// wins. Otherwise it builds one from cfg.EncryptionKey. Returns nil when
// neither is available (per-tenant DEK encryption stays disabled), logging the
// reason so the operator can tell "intentionally off" from "misconfigured".
func resolveKeyStore(o *routerOptions, cfg *config.Config) *encryption.KeyStore {
	if o.keyStore != nil {
		return o.keyStore
	}
	if cfg.EncryptionKey == "" {
		return nil
	}
	ks, err := encryption.NewKeyStore(cfg.EncryptionKey)
	if err != nil {
		slog.Warn("failed to create KeyStore from ENCRYPTION_KEY; per-tenant DEK encryption disabled", "err", err)
		return nil
	}
	return ks
}

func applyRouterOptions(opts []RouterOption) *routerOptions {
	o := &routerOptions{}
	for _, opt := range opts {
		opt(o)
	}
	// Fail closed: handlers always get a non-nil provider. With no license
	// wired this enforces unlicensed behavior, never lifts it.
	if o.entitlements == nil {
		o.entitlements = unlicensedEntitlements{}
	}
	return o
}

// adminSeats builds the guard every kernel path that writes roles asks before
// it adds an admin seat. The ceiling is optional, a licensing implementation
// may state it, and it is read from the entitlements on each write, so a
// changed ceiling applies without rebuilding the router.
func (o *routerOptions) adminSeats(users *db.UserStore) *db.AdminSeatGuard {
	return db.NewAdminSeatGuard(users,
		func() int { return o.entitlements.Snapshot().Caps[core.CapAdminSeats] },
		func() core.MembershipReader { return o.memberships })
}

// applyAccessLogging registers the middleware that must observe every request,
// ahead of everything that can refuse one.
//
// The access log sits above the guards that answer without calling next
// (PathSanitize 400, MaxBodySize 413, RateLimiter 429, IPAllowlist 403), so
// every refusal is logged and counted. A refusal is the request an operator
// most needs a record of: without the line, a rate limiter shedding traffic
// off an admin endpoint would look identical, in the log, to nobody having
// called it. The Prometheus counters hang off the same middleware.
//
// CorrelationID and the tracer run first so the line carries a request id and
// a trace id, and so a refusal answers with the same request id the log
// records. Registered before any route, on the outermost mux, so a route added
// later cannot be mounted outside it: chi panics on a Use() after a route, and
// every subrouter inherits this chain.
func applyAccessLogging(r *chi.Mux, serviceName string) {
	r.Use(apimw.CorrelationID())
	r.Use(tracing.HTTPMiddleware(serviceName))
	r.Use(structuredLogger)
}

// applyBuiltinMiddleware wires the config-driven middleware stack onto r.
//
// first is registered ahead of every guard below. Those guards refuse without
// calling next (MaxBodySize answers 413, RateLimiter 429, PathSanitize 400,
// IPAllowlist 403), so a middleware registered after them never runs on a
// refusal. Security headers belong in first for exactly that reason: they are
// most needed on the responses an attacker can provoke cheaply.
func applyBuiltinMiddleware(r *chi.Mux, cfg *config.Config, bodyOverrides map[string]int64, first ...func(http.Handler) http.Handler) {
	applyRouterFallbacks(r)

	for _, mw := range first {
		r.Use(mw)
	}

	// Path sanitization: reject null bytes, control characters, and
	// fullwidth Unicode homoglyphs before any handler sees the URL.
	r.Use(apimw.PathSanitize())

	// Payload size limiter: applied early to reject oversized requests quickly.
	// bodyOverrides lifts the ceiling for the handful of plugin routes that
	// declared a larger one. Every other path keeps cfg.MaxBodyBytes.
	r.Use(apimw.MaxBodySizeFor(cfg.MaxBodyBytes, bodyOverrides))

	// Per-IP rate limiter (only when enabled via RATE_LIMIT_RPS > 0).
	// Core ships an in-process token-bucket limiter only: it holds no Redis
	// client. Distributed (shared-counter) rate limiting comes from a plugin,
	// keeping Redis a plugin concern.
	if cfg.RateLimitRPS > 0 {
		if cfg.RateLimitBackend == "redis" {
			slog.Warn("rate_limiter: RATE_LIMIT_BACKEND=redis needs a plugin that supplies distributed rate limiting. " +
				"Using the in-process limiter.")
		}
		r.Use(apimw.RateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst))
	}

	// Response time header.
	r.Use(apimw.ResponseTime())

	// IP allowlist (no-op when empty).
	// Config refuses to load a malformed list, so an error here means the
	// config was built some other way. It fails closed: an allowlist that
	// cannot be read admits nobody, rather than admitting everybody.
	allowlist, err := apimw.IPAllowlist(cfg.IPAllowlist)
	if err != nil {
		slog.Error("invalid IP_ALLOWLIST config; refusing every request", "error", err)
		r.Use(func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				httpx.Error(w, http.StatusServiceUnavailable, "The IP allowlist is misconfigured.")
			})
		})
	} else {
		r.Use(allowlist)
	}

	// Response compression: gzip + brotli with content-type-aware levels.
	// JSON gets lower levels (gzip=4, brotli=2) for faster encoding since
	// JSON is already compact, while text/html gets the standard gzip=6/brotli=4.
	// Skips images, video, audio, and already-compressed formats.
	// Sets Vary: Accept-Encoding on every response.
	// Applied early so all handlers benefit, but after rate limiter so
	// 429 responses are not compressed (small, no benefit).
	r.Use(apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc))
}

// applyRouterFallbacks replaces the router's own two responses with the error
// envelope every handler already returns.
//
// Left to the defaults, an unrouted path answers net/http's plain-text
// "404 page not found" and a wrong method answers an empty 405. Those are the
// only responses the engine emits that a client cannot parse as
// {"error", "code", "request_id"}, and the only two carrying no request id to
// correlate against the log: reached, ironically, by the two mistakes a client
// is most likely to make against an unfamiliar API.
//
// Set before any route is registered: chi hands both to each subrouter as it is
// mounted, so one call here covers every route the router will grow.
func applyRouterFallbacks(r *chi.Mux) {
	r.NotFound(notFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		// chi carries the methods a 405 path accepts, but only its own
		// handler reads them: registering one of ours drops the Allow
		// header, so it is set here. A 405 without Allow tells a client it
		// guessed wrong and not what to guess instead, which is the whole
		// point of the status, and RFC 9110 requires the field.
		if allow := allowedMethods(r, req.URL.Path); allow != "" {
			w.Header().Set("Allow", allow)
		}
		httpx.ErrorReq(w, req, http.StatusMethodNotAllowed, "That method is not allowed on this endpoint.")
	})
}

func notFound(w http.ResponseWriter, req *http.Request) {
	httpx.ErrorReq(w, req, http.StatusNotFound, "The requested endpoint does not exist.")
}

// applyCustomRoutes offers an unrouted request to the custom route provider
// before the 404. Set right after applyRouterFallbacks and before any route:
// chi copies the handler into each subrouter as it is mounted and never
// updates one that already has its own, so a later call would reach only the
// paths no subrouter owns. Inside a subrouter the provider is asked too,
// which costs nothing: the provider refuses the prefixes the engine owns.
func applyCustomRoutes(r *chi.Mux, p core.CustomRouteProvider) {
	if p == nil {
		return
	}
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if h := p.CustomRoute(req); h != nil {
			h.ServeHTTP(w, req)
			return
		}
		notFound(w, req)
	})
}

// methodProbeOrder is the set a 405 is answered from, in the order the Allow
// header lists them. chi resolves HEAD and OPTIONS itself, so a path that
// accepts GET is reported as accepting HEAD whether or not one was registered.
var methodProbeOrder = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// allowedMethods asks the routing tree which methods the path does accept,
// as a ready-made Allow value. Empty when none do.
func allowedMethods(r *chi.Mux, path string) string {
	var allow []string
	for _, m := range methodProbeOrder {
		if r.Match(chi.NewRouteContext(), m, path) {
			allow = append(allow, m)
		}
	}
	return strings.Join(allow, ", ")
}

// newTenancyForEngine returns the appropriate Tenancy implementation for the
// current database engine. Postgres uses schema-per-tenant via search_path.
// MySQL and MSSQL use database-per-tenant via USE.
func newTenancyForEngine(pool db.DB, tenantIDFunc func(context.Context) string) db.Tenancy {
	switch pool.Engine() {
	case "mysql":
		return db.NewMySQLDatabaseTenancy(tenantIDFunc, db.DatabaseNameOf(pool))
	case "mssql":
		return db.NewMSSQLDatabaseTenancy(tenantIDFunc, db.DatabaseNameOf(pool))
	default:
		return db.NewPostgresSchemaTenancy(tenantIDFunc)
	}
}

// respond encodes body as JSON and writes status + Content-Type header.
// Uses jsonpool.WriteJSON to avoid encoder-internal buffer allocations on hot paths.
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = jsonpool.WriteJSON(w, body) // err suppressed: response write to client
	}
}

type errResponse struct {
	Error     string `json:"error"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// respondErrCode sends a translated error response using the i18n error code
// system. The locale is read from the request context (set by Locale middleware)
// and the message is resolved from the translation catalog with fallback chain
// applied. Extra params (map[string]any) are interpolated into the message
// template. The error code is included in the JSON response so API consumers
// can branch on it machine-readably.
func respondErrCode(w http.ResponseWriter, r *http.Request, status int, code i18n.Code, params map[string]any) {
	loc := i18n.LocaleFromCtx(r.Context())
	msg := code.Error(loc, params)
	respond(w, status, errResponse{
		Error:     msg,
		Code:      string(code),
		RequestID: logging.RequestIDFromCtx(r.Context()),
	})
}

// passwordViolationResponse maps a ValidatePassword error to the appropriate
// i18n code and interpolation parameters.
func passwordViolationResponse(err error, policy auth.PasswordPolicy) (i18n.Code, map[string]any) {
	switch {
	case errors.Is(err, auth.ErrPasswordTooShort):
		return i18n.CodePasswordTooShort, map[string]any{"min": policy.MinLength}
	case errors.Is(err, auth.ErrPasswordNoComplexity):
		return i18n.CodePasswordNeedsComplexity, nil
	case errors.Is(err, auth.ErrPasswordTooCommon):
		return i18n.CodePasswordTooCommon, nil
	default:
		return i18n.CodePasswordTooShort, nil
	}
}

// buildPublicRateLimiter creates a PublicEndpointRateLimiter over the table
// publicRateLimitTable assembles, or returns nil when that table holds no
// entry and no global cap.
func buildPublicRateLimiter(o *routerOptions) *apimw.PublicEndpointRateLimiter {
	configs, global := publicRateLimitTable(o)
	if len(configs) == 0 && global == nil {
		return nil
	}
	return apimw.NewPublicEndpointRateLimiter(configs, global, o.trustedProxies)
}

// publicRateLimitTable is what the public limiter enforces. Three layers
// merge in order, each over the one before: the engine's shipped rows, the
// limits the routes declare for themselves, and the operator's
// PUBLIC_RATE_LIMITS. A route's own declaration takes the place of the
// engine's row for it, and the operator's setting overrides both.
func publicRateLimitTable(o *routerOptions) (map[string]apimw.PublicRateLimitConfig, *apimw.PublicRateLimitConfig) {
	configs := o.publicRateLimits
	global := o.publicGlobalLimit
	if configs == nil {
		configs, global = apimw.DefaultPublicRateLimits()
	}
	if declared := DeclaredPublicRateLimits(o.declaredRouteSets()); len(declared) > 0 {
		configs = apimw.MergePublicRateLimits(configs, declared)
	}
	// Merging rather than replacing means raising one endpoint cannot take
	// the protection off the rest.
	if len(o.publicLimitOverrides) > 0 {
		configs = apimw.MergePublicRateLimits(configs, o.publicLimitOverrides)
	}
	if o.publicGlobalOverride != nil {
		global = o.publicGlobalOverride
	}
	return configs, global
}

// mountablePrefixes are the prefixes a plugin route can live under. One per
// router: NewAPIRouter mounts /api/v1, NewAdminRouter mounts /api/admin.
var mountablePrefixes = []string{"/api/v1", "/api/admin"}

// WarnUnmountableRoutes logs every plugin route that sits under no prefix any
// router mounts, every route whose group the kernel does not define, and
// every declared rate limit the public limiter will not apply.
//
// mountPluginRoutes filters by its own prefix, so neither router can tell the
// difference between "not mine" and "nobody's". A pattern under neither prefix
// is therefore dropped by both, in silence, and answers 404 to every caller
// with or without a credential, which reads as the handler being broken
// rather than never mounted. An unrecognized group is quieter still: it falls
// through to the authenticated arm, so a route meant to be public asks for a
// credential and nothing says why. A rate limit the limiter skips leaves the
// route on the engine's row or the global cap while its declaration says
// otherwise. A declared API key scope that cannot be read closes the route
// to every key short of the full wildcard, and one on a group that never
// reads it changes nothing, while both look like a working declaration.
//
// Called once at boot, where the whole set is visible.
func WarnUnmountableRoutes(pluginRoutes []plugin.PluginRoutes) {
	known := map[plugin.RouteGroup]bool{
		plugin.GroupPublic: true, plugin.GroupAuth: true,
		plugin.GroupAdmin: true, plugin.GroupSuperAdmin: true,
	}
	for _, pr := range pluginRoutes {
		for _, rd := range pr.Routes {
			mounted := false
			for _, p := range mountablePrefixes {
				if strings.HasPrefix(rd.Pattern, p) {
					mounted = true
					break
				}
			}
			if !mounted {
				slog.Warn("plugin route sits under no prefix any router mounts: it is dead and will answer 404",
					"plugin", pr.Name, "method", rd.Method, "pattern", rd.Pattern,
					"mountable_prefixes", mountablePrefixes)
			}
			if !known[rd.Group] {
				slog.Warn("plugin route declares an unknown group: it will be treated as authenticated",
					"plugin", pr.Name, "method", rd.Method, "pattern", rd.Pattern, "group", rd.Group)
			}
			if rd.Scope != "" {
				if known[rd.Group] && rd.Group != plugin.GroupAuth {
					slog.Warn("plugin route declares an API key scope outside the authenticated group: only that group reads it",
						"plugin", pr.Name, "method", rd.Method, "pattern", rd.Pattern, "group", rd.Group, "scope", rd.Scope)
				} else if _, ok := core.ParseScope(rd.Scope); !ok {
					slog.Warn("plugin route declares an API key scope that is not resource:action: only a key granted everything reaches it",
						"plugin", pr.Name, "method", rd.Method, "pattern", rd.Pattern, "scope", rd.Scope)
				}
			}
			if rd.RateLimit != nil && !appliedRateLimit(rd) {
				slog.Warn("plugin route declares a rate limit the public limiter will not apply: it wraps only public routes, and both the rate and the burst must be positive",
					"plugin", pr.Name, "method", rd.Method, "pattern", rd.Pattern, "group", rd.Group,
					"rate", rd.RateLimit.Rate, "burst", rd.RateLimit.Burst)
			}
		}
	}
}

// mountPluginRoutes mounts route declarations collected from the plugin
// activator onto the given chi.Router. prefix is stripped from each route's
// pattern before mounting (e.g. "/api/admin" for admin routes, "/api/v1"
// for API routes). Routes are mounted with middleware gating determined by
// RouteDecl.Group.
// sessionCurrent refuses an access token its user's token_version has moved
// past (logout, disable, a password set). The engine's own groups apply it.
// Without it a revoked session would keep working on plugin routes until it
// expired. Nil disables it, for tests that mount routes without a user store.
func mountPluginRoutes(r chi.Router, pluginRoutes []plugin.PluginRoutes, prefix string, csrfProtect bool, maxJSONBodyBytes int64, publicLimiter *apimw.PublicEndpointRateLimiter, publicTenantGuard func(http.Handler) http.Handler, sessionCurrent func(http.Handler) http.Handler) {
	mountDeclaredRoutes(r, pluginRoutes, nil, nil, prefix, csrfProtect, maxJSONBodyBytes, publicLimiter, publicTenantGuard, sessionCurrent)
}

// mountDeclaredRoutes mounts the plugins' routes and the routes of owners
// that are not plugins, as mountPluginRoutes describes. An owner's route goes
// into the same group stack as a plugin's route of that group. It is not
// wrapped in the tenant gate, because what a tenant can be denied is a
// plugin, and it carries no plugin label for the metrics.
//
// withholds is the licensing implementation's answer to whether a plugin is
// refused to a tenant, which the tenant gate asks on every plugin route. Nil
// refuses nothing.
func mountDeclaredRoutes(r chi.Router, pluginRoutes, ownerRoutes []plugin.PluginRoutes, withholds func(tenant, name string) bool, prefix string, csrfProtect bool, maxJSONBodyBytes int64, publicLimiter *apimw.PublicEndpointRateLimiter, publicTenantGuard func(http.Handler) http.Handler, sessionCurrent func(http.Handler) http.Handler) {
	if sessionCurrent == nil {
		sessionCurrent = func(next http.Handler) http.Handler { return next }
	}
	// Guardrail: warn when a plugin declares a route the engine already owns.
	// Plugin routes mount here, BEFORE the engine registers its own routes on
	// the same router, so chi's silent tree overwrite makes the engine handler
	// win and the plugin route is dead (never served). Log-only: a shadowed
	// route is harmless at runtime, but the dead config is confusing.
	for _, c := range engineRouteConflicts(pluginRoutes, prefix) {
		slog.Warn("plugin route shadowed by engine-owned route: engine handler wins, plugin route is dead; remove it from the plugin's Routes()",
			"plugin", c.plugin, "method", c.method, "pattern", c.pattern)
	}
	for _, c := range engineRouteConflicts(ownerRoutes, prefix) {
		slog.Warn("declared route shadowed by engine-owned route: engine handler wins, the declared route is dead",
			"owner", c.plugin, "method", c.method, "pattern", c.pattern)
	}

	type pluginRoute struct {
		decl   plugin.RouteDecl
		plugin string
		owned  bool // the owner is not a plugin
	}
	var publicRoutes, authRoutes, adminRoutes, superAdminRoutes []pluginRoute

	collect := func(sets []plugin.PluginRoutes, owned bool) {
		for _, pr := range sets {
			for _, rd := range pr.Routes {
				if !strings.HasPrefix(rd.Pattern, prefix) {
					continue
				}
				prt := pluginRoute{decl: rd, plugin: pr.Name, owned: owned}
				switch rd.Group {
				case plugin.GroupPublic:
					publicRoutes = append(publicRoutes, prt)
				case plugin.GroupAdmin:
					adminRoutes = append(adminRoutes, prt)
				case plugin.GroupSuperAdmin:
					superAdminRoutes = append(superAdminRoutes, prt)
				default:
					authRoutes = append(authRoutes, prt)
				}
			}
		}
	}
	collect(pluginRoutes, false)
	collect(ownerRoutes, true)

	// A plugin's route runs behind the tenant gate and under the plugin's
	// label. An owned route runs its handler as declared. The super admin
	// group is never gated, so it takes only the label.
	gated := func(prt pluginRoute) http.Handler {
		if prt.owned {
			return prt.decl.Handler
		}
		return withPluginLabel(prt.plugin, withTenantGate(withholds, prt.plugin, prt.decl.Handler))
	}
	labeled := func(prt pluginRoute) http.Handler {
		if prt.owned {
			return prt.decl.Handler
		}
		return withPluginLabel(prt.plugin, prt.decl.Handler)
	}

	// Body-size guard for JSON API routes. The global MaxBodySize middleware
	// provides the hard limit (10 MiB, covers file uploads).
	//
	// Multipart is exempt, so an upload route keeps the limit it declares.
	// Plugin upload routes are mounted in these same groups, and
	// ContentLengthLimit wraps r.Body, so the JSON cap would otherwise refuse
	// every upload over the 1 MiB default with 413.
	jsonGuard := apimw.JSONContentLengthLimit(maxJSONBodyBytes)

	// A route that declared its own ceiling is guarded at that ceiling instead
	// of the group's. Without this the declaration would only ever narrow the
	// body, never widen it, and the route would still be refused below the
	// size it publishes.
	guardFor := func(decl plugin.RouteDecl) func(http.Handler) http.Handler {
		if decl.MaxBodyBytes > 0 {
			return apimw.ContentLengthLimit(decl.MaxBodyBytes)
		}
		return jsonGuard
	}

	if len(publicRoutes) > 0 {
		r.Group(func(sub chi.Router) {
			for _, prt := range publicRoutes {
				rel := strings.TrimPrefix(prt.decl.Pattern, prefix)
				handler := gated(prt)
				if publicLimiter != nil {
					handler = publicLimiter.Wrap(prt.decl.Method, prt.decl.Pattern, handler)
				}
				mws := []func(http.Handler) http.Handler{guardFor(prt.decl)}
				// A public request names its tenant by the Host, so one that
				// resolves none on an install with several is refused here
				// rather than reaching a store that would answer with the
				// wrong tenant's rows or none at all.
				//
				// Mounted per route rather than on the group because the
				// exemption is a property of the route: a self-scoping route
				// carries its tenant in something signed, and the handler that
				// verifies it has not run yet at this point in the chain.
				//
				// This runs after routing, which is what lets it read the
				// declaration at all. The tenant it checks was resolved by
				// TenantHeader, which is mounted on the whole router and has
				// therefore already run.
				if publicTenantGuard != nil && !prt.decl.SelfScoping {
					mws = append(mws, publicTenantGuard)
				}
				sub.With(mws...).Method(prt.decl.Method, rel, handler)
			}
		})
	}

	if len(authRoutes) > 0 {
		r.Group(func(sub chi.Router) {
			sub.Use(requireAuth)
			sub.Use(sessionCurrent)
			if csrfProtect {
				sub.Use(apimw.CSRFCheck)
			}
			// Every authenticated route either router serves holds an API key
			// to a scope, so a declared route is held to one too. Otherwise
			// the same route would answer a key differently depending on
			// whether a plugin or the kernel owns it. The scope is the one
			// the route declares, or else the one its path names, so a
			// route that declares nothing is never open to a key.
			//
			// A handler may narrow a key further, as a realtime stream does
			// by topic. It is never the only check.
			for _, prt := range authRoutes {
				rel := strings.TrimPrefix(prt.decl.Pattern, prefix)
				sub.With(guardFor(prt.decl), apimw.RequireScope(routeScope(prt.decl))).Method(prt.decl.Method, rel, gated(prt))
			}
		})
	}

	if len(adminRoutes) > 0 {
		r.With(apimw.RequireScoped, requireRole("admin", "super_admin"), sessionCurrent).Group(func(sub chi.Router) {
			if csrfProtect {
				sub.Use(apimw.CSRFCheck)
			}
			for _, prt := range adminRoutes {
				rel := strings.TrimPrefix(prt.decl.Pattern, prefix)
				sub.With(guardFor(prt.decl)).Method(prt.decl.Method, rel, gated(prt))
			}
		})
	}

	if len(superAdminRoutes) > 0 {
		r.With(apimw.RequireScoped, requireRole("super_admin"), sessionCurrent).Group(func(sub chi.Router) {
			if csrfProtect {
				sub.Use(apimw.CSRFCheck)
			}
			for _, prt := range superAdminRoutes {
				rel := strings.TrimPrefix(prt.decl.Pattern, prefix)
				sub.With(guardFor(prt.decl)).Method(prt.decl.Method, rel, labeled(prt))
			}
		})
	}
}

// routeScope is the scope an API key needs to call a declared route: the
// scope the route declares, or else the one the request's path names. A
// declared scope that is not resource:action reports false, which the scope
// gate reads as a route only the full wildcard reaches.
func routeScope(decl plugin.RouteDecl) func(*http.Request) (core.ScopePair, bool) {
	if decl.Scope == "" {
		return apimw.PathScope
	}
	pair, ok := core.ParseScope(decl.Scope)
	return func(*http.Request) (core.ScopePair, bool) { return pair, ok }
}

// withPluginLabel wraps an http.Handler to inject the plugin name into the
// request context so the metrics middleware can label observations.
func withPluginLabel(plugin string, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The slot carries the name back out to the request sampler and the
		// access log, which wrap the whole chain on a context this frame
		// derives from and they never see.
		core.PluginSlotFrom(r.Context()).Set(plugin)
		ctx := context.WithValue(r.Context(), metrics.PluginCtxKey, plugin)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// declaredMediaTypes builds the content type guard's admission for the
// media types routes declared. The guard runs ahead of routing, so it asks
// mux which pattern the request will be served under, reading the raw path
// as the router does, and admits a declared type only when that pattern is
// exactly the declaring route's. A path a declaration merely resembles, an
// encoded separator that routes elsewhere, or an engine route that outranks
// the declared one is served under a different pattern and keeps the rule.
//
// A plugin route the engine shadows is never served, so its declaration is
// dropped rather than lent to the engine route that wins.
func declaredMediaTypes(mux routeFinder, sets []plugin.PluginRoutes) apimw.ContentTypeOption {
	shadowed := map[string]bool{}
	for _, prefix := range mountablePrefixes {
		for _, c := range engineRouteConflicts(sets, prefix) {
			shadowed[c.plugin+" "+grantKey(c.method, c.pattern)] = true
		}
	}
	declared := map[string]map[string]bool{}
	for _, pr := range sets {
		for _, rd := range pr.Routes {
			if len(rd.MediaTypes) == 0 {
				continue
			}
			key := grantKey(rd.Method, rd.Pattern)
			if shadowed[pr.Name+" "+key] {
				slog.Warn("route declares MediaTypes but an engine route shadows it: the declaration is ignored",
					"owner", pr.Name, "method", rd.Method, "pattern", rd.Pattern)
				continue
			}
			if declared[key] == nil {
				declared[key] = map[string]bool{}
			}
			for _, t := range rd.MediaTypes {
				declared[key][strings.ToLower(strings.TrimSpace(t))] = true
			}
		}
	}
	if len(declared) == 0 {
		return apimw.AllowMediaTypesWhen(nil)
	}
	return apimw.AllowMediaTypesWhen(func(r *http.Request, mediaType string) bool {
		pattern := routePatternOf(mux, r)
		return pattern != "" && declared[grantKey(r.Method, pattern)][mediaType]
	})
}

// pluginBodyOverrides collects the per-route body ceilings plugins declared, in
// the "METHOD /path" form the global body guard matches on.
//
// That guard runs ahead of routing, so it can only recognize a literal path. A
// declaration on a pattern holding a path parameter is reported and dropped
// rather than half-applied: the route would pass its own group guard and then
// be refused by the global one at the smaller limit, which is the fiction this
// mechanism exists to remove.
func pluginBodyOverrides(pluginRoutes []plugin.PluginRoutes) map[string]int64 {
	out := map[string]int64{}
	for _, pr := range pluginRoutes {
		for _, rd := range pr.Routes {
			if rd.MaxBodyBytes <= 0 {
				continue
			}
			if strings.ContainsRune(rd.Pattern, '{') {
				slog.Error("plugin route declares MaxBodyBytes on a pattern with a path parameter: the global body guard matches literal paths only, so the declaration is ignored and the route keeps the group limit",
					"plugin", pr.Name, "method", rd.Method, "pattern", rd.Pattern)
				continue
			}
			key := apimw.BodyLimitKey(rd.Method, rd.Pattern)
			if n, ok := out[key]; !ok || rd.MaxBodyBytes > n {
				out[key] = rd.MaxBodyBytes
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// routeConflict identifies a plugin route that collides with an engine-owned route.
type routeConflict struct {
	plugin  string
	method  string
	pattern string
}

// engineRouteConflicts returns the plugin routes (under prefix) whose
// method+pattern collide with a route the engine registers directly. Such
// routes are dead: the engine mounts its handler after plugin routes, so chi's
// tree overwrite makes the engine handler win. Path parameter names are
// normalized before comparison ({name} vs {id} route to the same chi node).
func engineRouteConflicts(pluginRoutes []plugin.PluginRoutes, prefix string) []routeConflict {
	owned := engineOwnedRoutes(prefix)
	if len(owned) == 0 {
		return nil
	}
	var conflicts []routeConflict
	for _, pr := range pluginRoutes {
		for _, rd := range pr.Routes {
			if !strings.HasPrefix(rd.Pattern, prefix) {
				continue
			}
			key := strings.ToUpper(rd.Method) + " " + normalizeRoutePattern(rd.Pattern)
			if _, ok := owned[key]; ok {
				conflicts = append(conflicts, routeConflict{plugin: pr.Name, method: rd.Method, pattern: rd.Pattern})
			}
		}
	}
	return conflicts
}

// normalizeRoutePattern rewrites named path parameters to a positional
// placeholder ({name}, {id}, {id:[0-9]+} -> {}) so two patterns that differ
// only in their parameter names compare equal (chi routes them to the same
// tree node, so they collide).
func normalizeRoutePattern(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// engineOwnedRoutes returns the set of "METHOD <normalized-pattern>" routes the
// core engine registers directly under the given router prefix ("/api/admin" or
// "/api/v1"). This mirrors the r.Get/r.Post/... calls in NewAdminRouter and
// NewAPIRouter and MUST be kept in sync with them. A router test walks the
// admin router and fails on any route missing here. It backs the guardrail in
// mountPluginRoutes that warns when a plugin re-declares an engine-owned
// route, and the admin grant registry, which refuses a plugin grant on one.
// The pprof handlers answer every method and are left out: their paths are
// session only, which refuses a grant on them already.
func engineOwnedRoutes(prefix string) map[string]struct{} {
	var patterns []string
	switch prefix {
	case "/api/admin":
		patterns = []string{
			"GET /api/admin/setup",
			"POST /api/admin/setup",
			"POST /api/admin/auth/login",
			"POST /api/admin/auth/refresh",
			"POST /api/admin/auth/logout",
			"POST /api/admin/auth/mfa-verify",
			"GET /api/admin/auth/me",
			"GET /api/admin/users",
			"POST /api/admin/users",
			"PUT /api/admin/users/{id}/roles",
			"PUT /api/admin/users/{id}/state",
			"PUT /api/admin/users/{id}/password",
			"DELETE /api/admin/users/{id}",
			"GET /api/admin/openapi.json",
			"GET /api/admin/openapi/public.json",
			"GET /api/admin/openapi/admin.json",
			"GET /api/admin/plugins/status",
			"GET /api/admin/plugins/running",
			"GET /api/admin/security/controls",
			"GET /api/admin/plugins/{name}/schema",
			"GET /api/admin/pool/health",
			"GET /api/admin/entitlements",
			"POST /api/admin/gdpr/export",
			"POST /api/admin/gdpr/erase",
			"GET /api/admin/health",
			"GET /api/admin/ready",
			"GET /api/admin/metrics",
			"GET /api/admin/debug/latency",
			"GET /api/admin/debug/gc-config",
			"POST /api/admin/debug/gc-config",
			"GET /api/admin/debug/goroutines",
			"GET /api/admin/auth/memberships",
			"GET /api/admin/schemas/export",
			"POST /api/admin/schemas/import",
			"GET /api/admin/users/{id}/memberships",
			"PUT /api/admin/users/{id}/memberships",
			"DELETE /api/admin/users/{id}/memberships/{tenant}",
			"GET /api/admin/tenant-members/{tenant}",
			"GET /api/admin/config",
			"GET /api/admin/api-key-scopes",
			"PUT /api/admin/config",
			"GET /api/admin/plugins/{name}/config",
			"PUT /api/admin/plugins/{name}/config",
			"POST /api/admin/plugins/{name}/config/reset",
			"GET /api/admin/admin-tokens",
			"GET /api/admin/admin-tokens/grants",
			"POST /api/admin/admin-tokens",
			"POST /api/admin/admin-tokens/{id}/rotate",
			"DELETE /api/admin/admin-tokens/{id}",
			"GET /api/admin/admin-tokens/{id}/requests",
			"POST /api/admin/auth/device",
			"POST /api/admin/auth/device/token",
			"GET /api/admin/auth/device/{user_code}",
			"POST /api/admin/auth/device/{user_code}/approve",
			"POST /api/admin/auth/device/{user_code}/deny",
		}
	case "/api/v1":
		patterns = []string{
			"POST /api/v1/auth/token",
			"GET /api/v1/health",
			"GET /api/v1/ready",
			"GET /api/v1/schemas",
			"GET /api/v1/schemas/{name}",
			"GET /api/v1/content/{schema}",
			"GET /api/v1/content/{schema}/cursor",
			"GET /api/v1/content/{schema}/stream",
			"GET /api/v1/content/{schema}/{id}",
			"GET /api/v1/content/{schema}/{id}/relations/{field}",
			"GET /api/v1/content/{schema}/{id}/revisions",
			"POST /api/v1/content/{schema}",
			"POST /api/v1/content/{schema}/bulk",
			"PUT /api/v1/content/{schema}/{id}",
			"DELETE /api/v1/content/{schema}/{id}",
			"PUT /api/v1/content/{schema}/{id}/relations/{field}",
			"PUT /api/v1/content/{schema}/{id}/publish",
			"PUT /api/v1/content/{schema}/{id}/unpublish",
			"PUT /api/v1/content/{schema}/{id}/revisions/{rev_id}/restore",
		}
	default:
		return nil
	}
	set := make(map[string]struct{}, len(patterns))
	for _, p := range patterns {
		method, pat, ok := strings.Cut(p, " ")
		if !ok {
			continue
		}
		set[method+" "+normalizeRoutePattern(pat)] = struct{}{}
	}
	return set
}

// schemaEngineAbsent reports whether this install has no schema engine. The
// absent source is the only way that state is expressed, so a caller reads it
// by asking the source rather than by tracking a flag beside it.
//
// The answer is read again on every call rather than once at build, because
// the source the routers are given resolves per read and a plugin registers
// its engine before the routers exist but could stop afterwards.
func schemaEngineAbsent(src core.SchemaSource) bool {
	return core.NoSchemaEngine(src)
}

// warnNoSchemaEngine says once, at boot, what an install without a schema
// engine cannot do. The routes answer the same thing per request, but an
// operator reading a log wants to find it without making a request first.
func warnNoSchemaEngine() {
	slog.Warn("no schema engine is installed",
		"effect", "content types cannot be defined, changed or served",
		"remedy", "install a plugin that registers a schema engine")
}
