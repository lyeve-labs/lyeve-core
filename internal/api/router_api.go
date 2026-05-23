package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/debug"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// NewAPIRouter builds the public content API HTTP router with JWT auth, CORS,
// tenant isolation, and plugin routes. External apps authenticate via Bearer token.
// Pass nil for hookRegistry to use a fresh empty registry.
func NewAPIRouter(pool db.DB, cfg *config.Config, hookRegistry *hooks.Registry, opts ...RouterOption) (http.Handler, error) {
	if hookRegistry == nil {
		hookRegistry = hooks.NewRegistry()
	}
	o := applyRouterOptions(opts)

	r := chi.NewRouter()
	root := r
	// Access logging first, so every response the router produces is recorded,
	// including the ones the guards below refuse without reaching a handler.
	applyAccessLogging(r, "cms-api")
	// Built-in middleware (rate limiter, response time, etc.). The caller's
	// address is resolved ahead of it, so the per-address limiter and the IP
	// allowlist see the client rather than the proxy in front of the engine.
	applyBuiltinMiddleware(r, cfg, pluginBodyOverrides(o.declaredRouteSets()),
		apimw.APISecurityHeaders(cfg.SecureCookie),
		apimw.StripUntrustedProxyHeaders(o.trustedProxies),
		apimw.ClientAddress(o.trustedProxies, cfg.ConsoleKeys()...))
	applyCustomRoutes(r, o.customRoutes)
	r.Use(middleware.Recoverer)
	r.Use(apimw.AllowedHosts(cfg.AllowedHosts))
	r.Use(apimw.HTTPSRedirect(cfg.SecureCookie))
	// Content-Type enforcement: rejects mutation requests (POST/PUT/PATCH)
	// that carry a non-JSON content type. Multipart/form-data is allowed
	// for file-upload endpoints. GET/DELETE/OPTIONS/HEAD pass through.
	// Placed AFTER APISecurityHeaders so 415 responses carry security headers.
	r.Use(apimw.RequireJSONContentType(declaredMediaTypes(r, o.declaredRouteSets())))
	r.Use(apimw.Locale)
	r.Use(corsMiddleware(CORSConfig{
		Origins:          cfg.CORSOrigins,
		AllowCredentials: false,
		PreflightMaxAge:  cfg.CORSPreflightMaxAge,
		AllowMethods:     cfg.CORSAllowMethods,
		AllowHeaders:     cfg.CORSAllowHeaders,
		ExposeHeaders:    cfg.CORSExposeHeaders,
		AllowedDomains:   cfg.CORSAllowedDomains,
	}))
	// Pre-auth middleware: a plugin implementing
	// core.PreAuthMiddlewareProvider injects tenant context here, before JWT
	// auth and TenantHeader consume it. See WithPreAuthMiddleware.
	for _, mw := range o.preAuth {
		r.Use(mw)
	}
	// An admin token is for the admin API. Here it is refused outright,
	// on every route, rather than read as no credential.
	r.Use(refuseAdminTokens)

	// Kubernetes health probes
	if o.healthProbes != nil {
		r.Get("/healthz", healthzHandler(o.healthProbes))
		r.Get("/readyz", readyzHandler(o.healthProbes))
		r.Get("/startup", startupHandler(o.healthProbes))
	}

	// JWKS
	// Public and unauthenticated: an external service verifying a CMS-issued
	// token has no token of its own to present. Mounted on the outer router,
	// before JWT auth, for the same reason the probes are. auth.JWKSHandler
	// answers with an empty keyset when no Ed25519 key is active, which is a
	// valid JWKS rather than an error.
	r.Get("/.well-known/jwks.json", auth.JWKSHandler())

	// The registry the content path reads. It is whatever engine a plugin
	// registered, and no engine at all when none has, which every content
	// route answers as 503 naming the reason rather than an empty page.
	contentSchemas := o.schemaSource
	if contentSchemas == nil {
		contentSchemas = core.AbsentSchemaSource{}
	}
	if schemaEngineAbsent(contentSchemas) {
		warnNoSchemaEngine()
	}
	schemaPresent := func() bool { return !schemaEngineAbsent(contentSchemas) }

	// The kernel's content caches stay in process memory whatever
	// CACHE_DRIVER says. A shared backend would not help: a write flushes the
	// list cache whole, which on a shared backend flushes every key in it.
	// Replicas keep their memory caches coherent through the instance bus.
	cacheCfg := cache.CacheConfig{
		Driver:     "memory",
		TTL:        cfg.CacheTTL,
		MaxEntries: cfg.CacheMaxEntries,
	}
	itemCache, ierr := cache.New[*domain.Content](cacheCfg, "content:item")
	if ierr != nil {
		slog.Error("failed to init item cache; falling back to no-cache", "error", ierr)
		itemCache = nil
	}
	listCache, lerr := cache.New[[]*domain.Content](cacheCfg, "content:list")
	if lerr != nil {
		slog.Error("failed to init list cache; falling back to no-cache", "error", lerr)
		listCache = nil
	}
	contentStore := db.NewContentStoreWithCache(pool, contentSchemas, itemCache, listCache)
	// Hand the same instance to the plugin host: a plugin that writes content
	// must invalidate the very caches these reads are served from.
	if o.contentStoreSink != nil {
		o.contentStoreSink(contentStore)
	}
	// A schema edit never touches a content write path, so nothing here drops
	// the rows cached against the old column set. The plugin that edits a
	// schema drops the content caches through the shared content store.
	userStore := db.NewUserStore(pool)
	// Auto-mounted via RoutesPlugin.

	contentHandler := &ContentHandler{store: contentStore, schemas: contentSchemas, hooks: hookRegistry, perms: o.permissionChecker, localizer: o.contentLocalizer, revisions: o.recordRevisions}

	secureCook := cfg.SecureCookie
	authHandler := NewAuthHandler(userStore, o.mfaStore, pool, cfg.JWTSecret, cfg.JWTExpirySecs, secureCook)
	authHandler.WithSharedLockoutBackend(o.lockoutBackend)
	authHandler.WithMemberships(o.memberships)
	authHandler.WithPasswordPolicy(auth.PasswordPolicy{
		MinLength:         cfg.PasswordMinLength,
		RequireUpperLower: cfg.PasswordRequireComplexity,
		RequireDigit:      cfg.PasswordRequireComplexity,
		CheckCommon:       cfg.PasswordCheckCommon,
	})
	// Wire per-tenant DEK encryption. Prefer a KeyStore injected by the runtime
	// (KEK resolved through the secret-custody chain). Otherwise fall back to
	// building one from ENCRYPTION_KEY.
	if ks := resolveKeyStore(o, cfg); ks != nil {
		authHandler.WithKeyStore(ks)
	}

	r.Route("/api/v1", func(r chi.Router) {
		// A path a flow claimed under /api/v1 is answered before this
		// prefix's chain, when no route here matches it: it is public and
		// finds its tenant from the path, like one outside the prefix.
		r.Use(customRoutesFirst(r, o.customRoutes))
		// Auth middlewares scoped to /api/v1.
		r.Use(jwtAuth(cfg.JWTSecrets, cfg.SecureCookie))
		// Trusted external issuers (cross-service JWT trust).
		r.Use(trustedIssuerAuth(cfg.TrustedIssuerPolicies, userStore, o.adminSeats(userStore)))
		// API key auth: runs after JWT. Injects claims when X-API-Key header
		// present. Passed via WithAPIKeyAuth from the runtime.
		if o.apiKeyAuth != nil {
			r.Use(o.apiKeyAuth)
		}
		// Every request a key makes is recorded here, straight after the key
		// is read, so a refusal and a plugin route are logged as well as the
		// engine's own routes.
		if o.apiKeyAudit != nil {
			r.Use(o.apiKeyAudit)
		}
		// Tenant isolation. TenantHeader resolves the slug into ctx, and TenancyConn
		// then acquires a per-request *sql.Conn, applies engine-appropriate
		// isolation (Postgres: SET search_path, MySQL/MSSQL: USE database), and
		// stashes it on ctx so every DB query inside the request stays in the
		// tenant scope (a sql.Conn-based middleware, not a pool-acquire hook).
		if o.tenantValidator != nil {
			r.Use(apimw.TenantHeader(cfg.MultiTenant, o.tenantValidator))
		} else {
			r.Use(apimw.TenantHeader(cfg.MultiTenant))
		}
		if cfg.MultiTenant {
			r.Use(apimw.TenancyConn(pool, newTenancyForEngine(pool, apimw.TenantIDFromContext)))
		}
		// ReadOnlyArchived: reject mutating requests against archived tenants
		// with HTTP 423 Locked. Only active when a tenant archived check is
		// supplied through WithArchivedChecker.
		if o.archivedChecker != nil {
			r.Use(apimw.ReadOnlyArchived(o.archivedChecker))
		}
		// Request capture: records full request/response for debugging.
		// Positioned after tenant/auth middleware so the captured entry
		// includes the resolved TenantID. The bodies of a route that declared
		// them sensitive stay out of it. The lookup asks the outer router,
		// which knows every pattern under this prefix.
		r.Use(apimw.RequestCaptureWithPolicy(o.requestCapture, sensitiveRoutes(root, o.declaredRouteSets()), o.capturePolicy))
		// Debug mode: X-Debug header gated behind admin role.
		// Replaces response body with timing breakdown report.
		r.Use(debug.Handler(cfg.DebugTracer, debug.AdminFromClaims()))
		// Caller-supplied extra middleware.
		for _, mw := range o.extra {
			r.Use(mw)
		}
		// The idempotency slot, filled through
		// core.IdempotencyMiddlewareProvider and mounted after every extra
		// middleware and before the routes, the position that interface
		// documents.
		if o.idempotency != nil {
			r.Use(o.idempotency)
		}

		// Mount the routes the plugins and the other owners declared.
		publicRL := buildPublicRateLimiter(o)
		mountDeclaredRoutes(r, o.pluginRoutes, o.ownerRoutes, o.entitlements.Withholds, "/api/v1", true, cfg.MaxJSONBodyBytes, publicRL, apimw.RequirePublicTenant(cfg.MultiTenant, o.tenantRoster), tokenVersionCheck(tokenVersionGetter(userStore)))

		// Public: obtain Bearer token (rate-limited to prevent brute-force).
		// Body-capped for the same reason as its admin counterpart: this is
		// reachable without a credential, so it needs the ceiling more than the
		// authenticated routes do.
		tokenGuard := apimw.ContentLengthLimit(cfg.MaxJSONBodyBytes)
		if publicRL != nil {
			r.With(tokenGuard, publicRL.WrapFunc("POST", "/api/v1/auth/token")).Post("/auth/token", authHandler.Token)
		} else {
			r.With(tokenGuard).Post("/auth/token", authHandler.Token)
		}

		// Auto-mounted via RoutesPlugin.

		// Operational health probes: any authenticated role.
		//
		// Mounted outside the scoped group below. RequireScoped derives the
		// required scope from the path, so a probe under it demands
		// "health:read", and an API key can only be issued with scopes the
		// operator knows to ask for. There is no scope catalog to learn that
		// name from, so a key minted for content work would answer 403 on a
		// probe that returns nothing but liveness. Authentication still applies.
		r.Group(func(r chi.Router) {
			r.Use(requireAuth)
			r.Use(tokenVersionCheck(tokenVersionGetter(userStore)))
			r.Get("/health", healthHandlerFn(pool))
			r.Get("/ready", readyHandlerFn(pool, schemaCount(contentSchemas), schemaPresent))
		})

		// All routes below require auth (JWT or API key).
		r.Group(func(r chi.Router) {
			r.Use(requireAuth)
			r.Use(tokenVersionCheck(tokenVersionGetter(userStore)))
			r.Use(apimw.CSRFCheck)
			r.Use(apimw.ContentLengthLimit(cfg.MaxJSONBodyBytes))
			// API key scope enforcement: gates API key requests based on their
			// scopes. JWT requests are unaffected.
			r.Use(apimw.RequireScoped)

			// Schema: read-only for all API consumers. The authoring routes
			// belong to whichever plugin registered the engine. These stay,
			// because a client reading content has to discover what it may
			// read without holding admin rights, and they answer 503 rather
			// than an empty list when no engine is registered.
			catalog := &schemaCatalog{source: o.schemaSource}
			r.With(apimw.CacheWithPolicy(apimw.DefaultSchemaPolicy)).Get("/schemas", catalog.List)
			r.With(apimw.CacheWithPolicy(apimw.DefaultSchemaPolicy)).Get("/schemas/{name}", catalog.Get)

			// Content reads: any authenticated role. Cache with strong ETag
			// and stale-while-revalidate for CDN-friendly delivery.
			r.With(apimw.CacheWithPolicy(apimw.DefaultContentPolicy)).Get("/content/{schema}", contentHandler.List)
			r.With(apimw.CacheWithPolicy(apimw.DefaultContentPolicy)).Get("/content/{schema}/cursor", contentHandler.ListCursor)
			// Stream: no cache policy (SSE, chunked transfer).
			r.Get("/content/{schema}/stream", contentHandler.Stream)
			r.With(apimw.CacheWithPolicy(apimw.DefaultContentPolicy)).Get("/content/{schema}/{id}", contentHandler.Get)
			r.With(apimw.CacheWithPolicy(apimw.DefaultContentPolicy)).Get("/content/{schema}/{id}/relations/{field}", contentHandler.ListRelations)
			r.With(apimw.CacheWithPolicy(apimw.DefaultContentPolicy)).Get("/content/{schema}/{id}/revisions", contentHandler.ListRevisions)

			// Content writes: editor, admin, super_admin only
			r.With(requireRole("editor", "admin", "super_admin")).Post("/content/{schema}", contentHandler.Create)
			r.With(requireRole("editor", "admin", "super_admin")).Post("/content/{schema}/bulk", contentHandler.BulkCreate)
			r.With(requireRole("editor", "admin", "super_admin")).Put("/content/{schema}/{id}", contentHandler.Update)
			r.With(requireRole("editor", "admin", "super_admin")).Delete("/content/{schema}/{id}", contentHandler.Delete)
			r.With(requireRole("editor", "admin", "super_admin")).Put("/content/{schema}/{id}/relations/{field}", contentHandler.SetRelations)
			r.With(requireRole("editor", "admin", "super_admin")).Put("/content/{schema}/{id}/publish", contentHandler.Publish)
			r.With(requireRole("editor", "admin", "super_admin")).Put("/content/{schema}/{id}/unpublish", contentHandler.Unpublish)
			r.With(requireRole("editor", "admin", "super_admin")).Put("/content/{schema}/{id}/revisions/{rev_id}/restore", contentHandler.RestoreRevision)
		})

		// Verify every public route wrapped by the limiter has a matching
		// config entry. Missing config means the route has NO per-endpoint
		// rate limiting: a silent brute-force gap.
		if publicRL != nil {
			if err := publicRL.Verify(); err != nil {
				slog.Error("public rate-limit config gap: add missing entries or disable rate limiting explicitly", "err", err)
			}
		}
	})

	return r, nil
}
