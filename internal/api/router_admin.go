package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	runtimedebug "runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/debug"
	"github.com/lyeve-labs/lyeve-core/internal/metrics"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// NewAdminRouter builds the admin HTTP router with JWT auth, CORS, tenant isolation,
// and plugin routes. Used by the CMS binary for the admin-facing HTTP server.
func NewAdminRouter(pool db.DB, cfg *config.Config, opts ...RouterOption) (http.Handler, error) {
	o := applyRouterOptions(opts)

	r := chi.NewRouter()
	// Access logging first, so every response the router produces is recorded,
	// including the ones the guards below refuse without reaching a handler.
	applyAccessLogging(r, "cms-admin")
	// Built-in middleware (rate limiter, response time, etc.). The caller's
	// address is resolved ahead of it, so the per-address limiter and the IP
	// allowlist see the client rather than the proxy in front of the engine.
	applyBuiltinMiddleware(r, cfg, pluginBodyOverrides(o.declaredRouteSets()),
		apimw.CSPNonce(), apimw.SecurityHeaders(cfg.SecureCookie),
		apimw.StripUntrustedProxyHeaders(o.trustedProxies),
		apimw.ClientAddress(o.trustedProxies, cfg.ConsoleKeys()...))
	r.Use(middleware.Recoverer)
	r.Use(apimw.AllowedHosts(cfg.AllowedHosts))
	r.Use(apimw.HTTPSRedirect(cfg.SecureCookie))
	// Content-Type enforcement: rejects mutation requests (POST/PUT/PATCH)
	// that carry a non-JSON content type. Multipart/form-data is allowed
	// for file-upload endpoints. GET/DELETE/OPTIONS/HEAD pass through.
	// Placed AFTER SecurityHeaders so 415 responses carry security headers.
	// The schema import reads a YAML or JSON bundle, and the export writes YAML
	// by default, so that one route also admits the YAML media types.
	// A declared route may name further media types for itself, as a SAML
	// assertion consumer does for the form its identity provider posts.
	r.Use(apimw.RequireJSONContentType(declaredMediaTypes(r, o.declaredRouteSets()),
		apimw.AllowMediaTypes("/api/admin/schemas/import",
			"application/yaml", "application/x-yaml", "text/yaml")))
	// Locale detection: parse Accept-Language and stash on context.
	// Runs early so all downstream handlers see the resolved locale.
	r.Use(apimw.Locale)
	// allowCredentials=true so the browser can send HTTP-only cookies cross-origin.
	r.Use(corsMiddleware(CORSConfig{
		Origins:          cfg.CORSOrigins,
		AllowCredentials: true,
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
	r.Use(jwtAuth(cfg.JWTSecrets, cfg.SecureCookie))
	// Admin tokens. Read right after the session, so a token request carries
	// its owner's claims before any key, tenant or plugin middleware looks.
	// The grants the engine, the plugins and the other route owners declare
	// are checked here at build time: an unknown grant or one on a
	// session-only route stops the router from being built at all.
	grantRegistry, err := buildAdminGrantRegistry(o.pluginRoutes, o.ownerRoutes)
	if err != nil {
		return nil, fmt.Errorf("admin router: %w", err)
	}
	tokenStore := o.adminTokens
	finder, err := routeFinderOf(r)
	if err != nil {
		return nil, err
	}
	tokenAuth := &adminTokenAuth{
		tokens:  tokenStore,
		users:   db.NewUserStore(pool),
		members: o.memberships,
		trusted: o.trustedProxies,
		log:     newAdminTokenLog(o.lifetimeCtx(), tokenStore),
		routes:  finder,
		now:     time.Now,
	}
	r.Use(tokenAuth.authenticate)
	// API key auth: runs after JWT. Injects claims when the X-API-Key header
	// is present. Passed via WithAPIKeyAuth from the runtime. The API router
	// already applies this. Wiring it here makes a scoped key a first-class
	// caller on the admin port too.
	if o.apiKeyAuth != nil {
		r.Use(o.apiKeyAuth)
	}
	// Every request a key makes on the admin port is recorded, refusals
	// included. The admin port is where a key with an admin role does the
	// most.
	if o.apiKeyAudit != nil {
		r.Use(o.apiKeyAudit)
	}
	// A key holding an admin role is refused here: admin tokens are the
	// credential for the admin API.
	// Mounted after the key audit, so the refusal is in the key's log too.
	if o.apiKeyAuth != nil {
		r.Use(newAdminRoleKeyDeprecation(tokenAuth.routePattern).middleware)
	}
	// Request capture: records full request/response for debugging when a
	// CaptureSink is wired. Positioned after CORS so redirects aren't captured.
	// The bodies of a route that declared them sensitive stay out of it.
	r.Use(apimw.RequestCaptureWithPolicy(o.requestCapture, sensitiveRoutes(finder, o.declaredRouteSets()), o.capturePolicy))
	// Tenant isolation, ahead of the plugin middleware in o.extra.
	//
	// Every tenant-aware plugin middleware reads the tenant with
	// core.TenantIDFromCtx, and only TenantHeader puts it there, so it runs
	// ahead of o.extra. The API router does the same.
	//
	// TenancyConn is NOT wired here: admin routes use tenant_id as a column
	// filter against shared tables, not schema-per-tenant isolation.
	//
	// The health probes below are registered after this and pass straight
	// through: they carry no claims, and TenantHeader is a no-op without them.
	if o.tenantValidator != nil {
		r.Use(apimw.TenantHeader(cfg.MultiTenant, o.tenantValidator))
	} else {
		r.Use(apimw.TenantHeader(cfg.MultiTenant))
	}
	// An admin token reaches only the routes that declare a grant it holds.
	// Ahead of the plugin middleware, so a refused token spends no quota.
	r.Use(enforceGrants(grantRegistry))
	// Caller-supplied extra middleware.
	for _, mw := range o.extra {
		r.Use(mw)
	}
	// The idempotency slot: Idempotency-Key enforcement on the write routes,
	// filled through core.IdempotencyMiddlewareProvider. Mounted after every
	// extra middleware and before the routes, the position that interface
	// documents.
	if o.idempotency != nil {
		r.Use(o.idempotency)
	}

	secureCook := cfg.SecureCookie
	userStore := db.NewUserStore(pool)
	sessionCurrent := tokenVersionCheck(tokenVersionGetter(userStore))
	// The registry the console reads. It is whatever engine a plugin
	// registered, and nothing when none has, in which case the schema routes
	// are simply absent.
	adminSchemas := o.schemaSource
	if adminSchemas == nil {
		adminSchemas = core.AbsentSchemaSource{}
	}
	adminSchemaPresent := func() bool { return !schemaEngineAbsent(adminSchemas) }

	var pluginConfigStore *db.PluginConfigStore
	if pool != nil {
		pluginConfigStore = db.NewPluginConfigStore(pool).
			WithSealer(db.NewConfigSealer(cfg.EncryptionKey))
		// The portable engine settings join a configuration bundle through
		// the same registry every plugin's section does.
		if reg, ok := o.scalingHost.(core.ConfigSectionRegistrar); ok {
			reg.RegisterConfigSection(settingsSectionName, newSettingsSection(pluginConfigStore, o.pluginReloader, pool.Engine()))
		}
	}

	authHandler := NewAuthHandler(userStore, o.mfaStore, pool, cfg.JWTSecret, cfg.JWTExpirySecs, secureCook, cfg.PasswordHashAlgo)
	authHandler.WithMultiTenant(cfg.MultiTenant)
	authHandler.WithSharedLockoutBackend(o.lockoutBackend)
	authHandler.WithTenantRegistry(o.defaultTenant, o.tenantValidator)
	setupToken := o.setupToken
	if setupToken == nil {
		setupToken = NewSetupTokenFromEnv(cfg.SetupToken)
	}
	authHandler.WithSetupToken(setupToken)
	authHandler.WithMemberships(o.memberships)
	// The document describes what this router mounts, so it is built from the
	// same values the mounts are.
	specOptions := openAPIOptions{
		TenantFeatures:      o.licenseRouteServed(http.MethodGet, tenantFeaturesPattern),
		LicensePresentation: o.licenseRouteServed(http.MethodGet, licensing.PresentationPath),
		AdminTokens:         tokenStore != nil,
		OwnerRoutes:         o.documentedOwnerRoutes(),
		Ungated:             func(name string) bool { return o.entitlements.Plugin(name).Ungated },
	}
	authHandler.WithPasswordPolicy(auth.PasswordPolicy{
		MinLength:         cfg.PasswordMinLength,
		RequireUpperLower: cfg.PasswordRequireComplexity,
		RequireDigit:      cfg.PasswordRequireComplexity,
		CheckCommon:       cfg.PasswordCheckCommon,
	})
	if o.refreshTokenStore != nil {
		authHandler.WithRefreshTokenStore(o.refreshTokenStore, time.Duration(cfg.RefreshTokenTTL)*time.Second)
	}
	if o.deviceRiskAssessor != nil {
		authHandler.WithDeviceRiskAssessor(o.deviceRiskAssessor)
	}
	// Wire per-tenant DEK encryption. Prefer a KeyStore injected by the runtime
	// (KEK resolved through the secret-custody chain). Otherwise fall back to
	// building one from ENCRYPTION_KEY.
	if ks := resolveKeyStore(o, cfg); ks != nil {
		authHandler.WithKeyStore(ks)
	}
	usersHandler := NewUsersHandler(userStore, cfg.PasswordHashAlgo)
	usersHandler.seats = o.adminSeats(userStore)
	if o.refreshTokenStore != nil {
		usersHandler.revokeRefresh = o.refreshTokenStore.RevokeAllForUser
	}
	usersHandler.passwordPolicy = auth.PasswordPolicy{
		MinLength:         cfg.PasswordMinLength,
		RequireUpperLower: cfg.PasswordRequireComplexity,
		RequireDigit:      cfg.PasswordRequireComplexity,
		CheckCommon:       cfg.PasswordCheckCommon,
	}

	// Kubernetes health probes
	// Mounted on the outer router before JWT auth middleware: Kubernetes
	// probes don't carry tokens.
	if o.healthProbes != nil {
		r.Get("/healthz", healthzHandler(o.healthProbes))
		r.Get("/readyz", readyzHandler(o.healthProbes))
		r.Get("/startup", startupHandler(o.healthProbes))
	}

	// A crawler asks here before anything else, and this server has nothing
	// for it: every route past the login is authenticated. Answered on the
	// outer router, before auth, so the refusal is a 200 with a rule rather
	// than a 401 the crawler retries. X-Robots-Tag on every response covers
	// the crawlers that never ask.
	r.Get("/robots.txt", robotsHandler)

	r.Route("/api/admin", func(r chi.Router) {
		// TenantHeader is mounted on the outer chain above, so plugin handlers
		// and plugin middleware alike see the tenant scope. Without it,
		// tenant-scoped admin callers have empty TenantIDFromCtx ->
		// conditional tenant-guards skip -> cross-tenant read/write on shared
		// sys_* tables. super_admin with no tenant claim or with an
		// X-Tenant-ID header override still works: TenantHeader passes through
		// on empty, and the header override path preserves existing ops and
		// tooling behavior when a super_admin sets the header.

		// ReadOnlyArchived is wired here as well as on the API router, because
		// content, media, users and schemas are all authored here, which is
		// most of what "read-only" is supposed to stop. Tenant lifecycle is
		// exempt: restore is the way out of the archived state and cannot be
		// gated on it.
		if o.archivedChecker != nil {
			r.Use(exceptTenantLifecycle(apimw.ReadOnlyArchived(o.archivedChecker)))
		}

		// Mount the routes the plugins and the other owners declared.
		// Build public endpoint rate limiter from options (or defaults).
		publicRL := buildPublicRateLimiter(o)
		mountDeclaredRoutes(r, o.pluginRoutes, o.ownerRoutes, o.entitlements.Withholds, "/api/admin", true, cfg.MaxJSONBodyBytes, publicRL, apimw.RequirePublicTenant(cfg.MultiTenant, o.tenantRoster), sessionCurrent)

		// Public: setup + auth
		// Core auth endpoints are wrapped with the public rate limiter to
		// prevent brute-force attacks on login, token refresh, and MFA.
		//
		// The JSON body guard covers these public routes too, so an oversize
		// body stops at the router with 413 instead of reaching the decoder and
		// coming back as a malformed-body 400.
		jsonGuard := apimw.ContentLengthLimit(cfg.MaxJSONBodyBytes)
		r.Get("/setup", authHandler.SetupStatus)
		r.With(jsonGuard).Post("/setup", authHandler.Setup)
		if publicRL != nil {
			r.With(jsonGuard, publicRL.WrapFunc("POST", "/api/admin/auth/login")).Post("/auth/login", authHandler.Login)
			r.With(jsonGuard, publicRL.WrapFunc("POST", "/api/admin/auth/refresh")).Post("/auth/refresh", authHandler.Refresh)
			r.With(jsonGuard, publicRL.WrapFunc("POST", "/api/admin/auth/mfa-verify")).Post("/auth/mfa-verify", authHandler.MFAVerify)
		} else {
			r.With(jsonGuard).Post("/auth/login", authHandler.Login)
			r.With(jsonGuard).Post("/auth/refresh", authHandler.Refresh)
			r.With(jsonGuard).Post("/auth/mfa-verify", authHandler.MFAVerify)
		}

		// Device sign-in. The device's two routes are public and each has its
		// own limit. The decision routes below are a signed-in admin's.
		var deviceLogins *deviceLoginHandler
		if pool != nil {
			deviceLogins = &deviceLoginHandler{
				store:       db.NewDeviceLoginStore(pool),
				users:       userStore,
				auth:        authHandler,
				host:        o.scalingHost,
				consoleURL:  cfg.ConsoleURL,
				production:  cfg.IsProduction(),
				multiTenant: cfg.MultiTenant,
				now:         time.Now,
			}
			go deviceLogins.runPruner(o.lifetimeCtx())
			deviceGuard := apimw.ContentLengthLimit(deviceLoginMaxBody)
			start := r.With(deviceGuard)
			poll := r.With(deviceGuard)
			if publicRL != nil {
				start = start.With(publicRL.WrapFunc("POST", "/api/admin/auth/device"))
				poll = poll.With(publicRL.WrapFunc("POST", "/api/admin/auth/device/token"))
			}
			start.Post("/auth/device", deviceLogins.Start)
			poll.Post("/auth/device/token", deviceLogins.Token)
		}

		// Auto-mounted via RoutesPlugin with GroupPublic.

		// Operational health probes: any authenticated role, mounted outside
		// the scoped group so a content/media-scoped key is not denied liveness.
		// Authentication still applies. Mirrors the API router.
		r.Group(func(r chi.Router) {
			r.Use(requireAuth)
			r.Use(tokenVersionCheck(tokenVersionGetter(userStore)))
			r.Get("/health", healthHandlerFn(pool))
			r.Get("/ready", readyHandlerFn(pool, schemaCount(adminSchemas), adminSchemaPresent))
		})

		// Authenticated routes
		r.Group(func(r chi.Router) {
			r.Use(requireAuth)
			r.Use(tokenVersionCheck(tokenVersionGetter(userStore)))
			r.Use(apimw.CSRFCheck)
			r.Use(apimw.ContentLengthLimit(cfg.MaxJSONBodyBytes))
			// API key scope enforcement: gates X-API-Key requests by their
			// scopes. JWT requests pass through. Mirrors the API router.
			r.Use(apimw.RequireScoped)
			// Debug mode: when X-Debug: true header is present and caller
			// has admin role, response is replaced with timing breakdown.
			r.Use(debug.Handler(cfg.DebugTracer, debug.AdminFromClaims()))

			r.Get("/auth/me", authHandler.Me)
			r.Get("/auth/memberships", authHandler.Memberships)
			r.Post("/auth/logout", authHandler.Logout)
			// Which plugins serve the caller's tenant, for every signed-in
			// role, so a console shows each screen only while its plugin runs.
			r.Get("/plugins/running", runningPluginsHandler(o.pluginStatus, o.entitlements))

			// The schema write routes belong to the plugin that registered the
			// schema engine and mount with the plugin routes.

			// Moving content types between projects. Export is a read. Import
			// runs DDL, so it is held to super_admin even though editing one
			// schema is not.
			r.With(requireRole("admin", "super_admin")).Get("/schemas/export", schemaExportHandler(o.scalingHost))
			r.With(requireRole("super_admin")).Post("/schemas/import", schemaImportHandler(o.scalingHost))

			// User management: super_admin only
			r.With(requireRole("super_admin")).Get("/users", usersHandler.List)
			r.With(requireRole("super_admin")).Post("/users", usersHandler.Create)
			r.With(requireRole("super_admin")).Put("/users/{id}/roles", usersHandler.UpdateRoles)
			r.With(requireRole("super_admin")).Put("/users/{id}/state", usersHandler.UpdateState)
			r.With(requireRole("super_admin")).Put("/users/{id}/password", usersHandler.SetPassword)
			r.With(requireRole("super_admin")).Delete("/users/{id}", usersHandler.Delete)

			// Auto-mounted via RoutesPlugin.

			// Auto-mounted via RoutesPlugin with GroupAdmin.

			// Admin tokens. Session only, like every route that issues a
			// credential: a key or a token calling one is refused.
			if tokenStore != nil {
				tokens := &adminTokenHandler{
					store:    tokenStore,
					users:    userStore,
					auth:     authHandler,
					members:  o.memberships,
					registry: grantRegistry,
					host:     o.scalingHost,
					validate: o.tenantValidator,
					now:      time.Now,
				}
				admins := requireRole("admin", "super_admin")
				r.With(admins).Get("/admin-tokens", tokens.List)
				r.With(admins).Get("/admin-tokens/grants", tokens.Grants)
				r.With(admins).Post("/admin-tokens", tokens.Create)
				r.With(admins).Post("/admin-tokens/{id}/rotate", tokens.Rotate)
				r.With(admins).Delete("/admin-tokens/{id}", tokens.Revoke)
				r.With(admins).Get("/admin-tokens/{id}/requests", tokens.Requests)
			}

			// Device sign-in decisions. Session only, like the admin token
			// routes: approving mints a person's session, so a key or a token
			// calling one is refused. The lookup is limited per address as
			// well, so a signed-in caller cannot walk the code space.
			if deviceLogins != nil {
				admins := requireRole("admin", "super_admin")
				lookup := r.With(admins)
				approve := r.With(admins)
				deny := r.With(admins)
				if publicRL != nil {
					lookup = lookup.With(publicRL.WrapFunc("GET", "/api/admin/auth/device/{user_code}"))
					approve = approve.With(publicRL.WrapFunc("POST", "/api/admin/auth/device/{user_code}/approve"))
					deny = deny.With(publicRL.WrapFunc("POST", "/api/admin/auth/device/{user_code}/deny"))
				}
				lookup.Get("/auth/device/{user_code}", deviceLogins.Lookup)
				approve.Post("/auth/device/{user_code}/approve", deviceLogins.Approve)
				deny.Post("/auth/device/{user_code}/deny", deviceLogins.Deny)
			}

			// OpenAPI spec. The public half for admins, the admin half for super
			// admins, and /openapi.json, which answers by role.
			r.With(requireRole("admin", "super_admin")).Get("/openapi.json", openAPIHandler(adminSchemas, o.pluginRoutes, o.endpointDocs, openAPIByRole, specOptions))
			r.With(requireRole("admin", "super_admin")).Get("/openapi/public.json", openAPIHandler(adminSchemas, o.pluginRoutes, o.endpointDocs, openAPIPublic, specOptions))
			r.With(requireRole("super_admin")).Get("/openapi/admin.json", openAPIHandler(adminSchemas, o.pluginRoutes, o.endpointDocs, openAPIAdmin, specOptions))
			// Reports whether each plugin is compiled, entitled and requested,
			// plus, for an inactive plugin, the link its licensing
			// implementation supplies.
			r.With(requireRole("admin", "super_admin")).Get("/plugins/status", pluginsStatusHandler(o.pluginStatus))

			// Plugin configuration JSON Schema for auto-generated admin forms.
			// Returns 404 when the plugin is not active or has no config schema.
			r.With(requireRole("admin", "super_admin")).Get("/plugins/{name}/schema", pluginSchemaHandler(o.pluginSchema))
			r.With(requireRole("super_admin")).Get("/config", configProvenanceHandler())
			// Every route an API key can be scoped to, for the key form. An
			// admin manages keys too, and the OpenAPI spec already lists these
			// routes to one.
			r.With(requireRole("admin", "super_admin")).Get("/api-key-scopes", keyScopesHandler(o.pluginRoutes, o.ownerRoutes))
			// The boot-time control liveness report, read live: which
			// protections are enforcing, which are compiled and off, and
			// what turns each one on.
			r.With(requireRole("super_admin")).Get("/security/controls", securityControlsHandler(o.securityControls))
			r.With(requireRole("super_admin")).Put("/config", configSaveHandler(pluginConfigStore, o.pluginReloader))
			r.With(requireRole("admin", "super_admin")).Get("/plugins/{name}/config", pluginConfigHandler(pluginConfigStore))
			r.With(requireRole("super_admin")).Put("/plugins/{name}/config", pluginConfigSaveHandler(pluginConfigStore, o.pluginSchema, o.pluginReloader))
			r.With(requireRole("super_admin")).Post("/plugins/{name}/config/reset", pluginConfigResetHandler(pluginConfigStore, o.pluginSchema, o.pluginReloader))

			// Pool health: returns live connection pool statistics.
			r.With(requireRole("admin", "super_admin")).Get("/pool/health", poolHealthHandler(o.poolHealthProvider))

			// Entitlements: admin or super_admin
			// Active plan, entitled features, and license state.
			// The admin reads this to hide what the license does not cover.
			r.With(requireRole("admin", "super_admin")).Get("/entitlements", entitlementsHandler(o.entitlements, o.licenseModule))

			// The license renewal and the tenant features pair are the
			// licensing implementation's, mounted with its own routes.

			// Auto-mounted via RoutesPlugin with GroupAdmin.

			// GDPR DSAR: Art.15 (access) / Art.17 (erasure) / Art.20 (portability)
			// Restricted to super_admin and audit-logged via DSARHandler.
			dsarHandler := NewDSARHandler(o.dsarAuditWriter, o.dsarTenants, o.dsarTenantScope) // wiring from the runtime
			// Rate-limited: an export reads every registered exporter and
			// returns the whole of one person's data, so a stolen session could
			// drain every tenant one identifier at a time with nothing to slow it.
			if publicRL != nil {
				r.With(requireRole("super_admin"), publicRL.WrapFunc("POST", "/api/admin/gdpr/export")).
					Post("/gdpr/export", dsarHandler.Export)
				r.With(requireRole("super_admin"), publicRL.WrapFunc("POST", "/api/admin/gdpr/erase")).
					Post("/gdpr/erase", dsarHandler.Erase)
			} else {
				r.With(requireRole("super_admin")).Post("/gdpr/export", dsarHandler.Export)
				r.With(requireRole("super_admin")).Post("/gdpr/erase", dsarHandler.Erase)
			}
		})

		r.With(sessionCurrent, metricsAuth(cfg.MetricsToken)).Get("/metrics", metrics.Handler().ServeHTTP)

		// pprof endpoints: super_admin only (sensitive memory/cpu data).
		// Mount at /api/admin/debug/pprof/ for Go tool compatibility:
		//   go tool pprof http://localhost:3001/api/admin/debug/pprof/heap
		r.With(sessionCurrent, requireRole("super_admin")).HandleFunc("/debug/pprof/", pprof.Index)
		r.With(sessionCurrent, requireRole("super_admin")).HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		r.With(sessionCurrent, requireRole("super_admin")).HandleFunc("/debug/pprof/profile", pprof.Profile)
		r.With(sessionCurrent, requireRole("super_admin")).HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		r.With(sessionCurrent, requireRole("super_admin")).HandleFunc("/debug/pprof/trace", pprof.Trace)
		r.With(sessionCurrent, requireRole("super_admin")).Handle("/debug/pprof/heap", pprof.Handler("heap"))
		r.With(sessionCurrent, requireRole("super_admin")).Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		r.With(sessionCurrent, requireRole("super_admin")).Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
		r.With(sessionCurrent, requireRole("super_admin")).Handle("/debug/pprof/block", pprof.Handler("block"))
		r.With(sessionCurrent, requireRole("super_admin")).Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
		r.With(sessionCurrent, requireRole("super_admin")).Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

		// Latency stats: admin or super_admin. Rankings of slowest endpoints.
		r.With(sessionCurrent, requireRole("admin", "super_admin")).Get("/debug/latency", latencyStatsHandler(o.latencyTracker))

		// GC config: read current GOGC and GOMEMLIMIT.
		r.With(sessionCurrent, requireRole("admin", "super_admin")).Get("/debug/gc-config", func(w http.ResponseWriter, r *http.Request) {
			var memLimit int64
			if ml := runtimedebug.SetMemoryLimit(-1); ml > 0 {
				memLimit = ml
			}
			respond(w, http.StatusOK, map[string]any{
				"gogc":         os.Getenv("GOGC"),
				"gomemlimit":   os.Getenv("GOMEMLIMIT"),
				"memory_limit": memLimit,
			})
		})

		// Update GC at runtime (no restart needed).
		r.With(sessionCurrent, requireRole("super_admin")).Post("/debug/gc-config", func(w http.ResponseWriter, r *http.Request) {
			var input struct {
				GOGC int `json:"gogc"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				respond(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
				return
			}
			old := runtimedebug.SetGCPercent(input.GOGC)
			respond(w, http.StatusOK, map[string]any{"previous_gogc": old, "current_gogc": input.GOGC})
		})

		// The views over the scaling primitives beside this snapshot, the
		// tunables behind them, and the request profiler are plugin routes
		// and mount with them.
		// Goroutine tracker snapshot.
		r.With(sessionCurrent, requireRole("admin", "super_admin")).Get("/debug/goroutines", func(w http.ResponseWriter, r *http.Request) {
			if o.scalingHost == nil {
				respond(w, http.StatusOK, map[string]any{"total": runtime.NumGoroutine()})
				return
			}
			tracker := o.scalingHost.GoroutineTracker()
			if tracker == nil {
				respond(w, http.StatusOK, map[string]any{"total": runtime.NumGoroutine()})
				return
			}
			respond(w, http.StatusOK, tracker.Snapshot())
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

// exceptTenantLifecycle lets requests under /api/admin/tenants bypass mw.
// Archive, restore and delete are how an operator manages the archived state,
// so gating them on it would strand a tenant with no way back.
func exceptTenantLifecycle(mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		guarded := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/admin/tenants") {
				next.ServeHTTP(w, r)
				return
			}
			guarded.ServeHTTP(w, r)
		})
	}
}
