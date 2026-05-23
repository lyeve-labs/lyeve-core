package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// StatelessMode is what GET /api/admin/mode reports on an engine running with
// no database: the mode itself, that nothing is metered or kept, and the API
// keys the configuration declares, without their hashes.
type StatelessMode struct {
	Mode     string                `json:"mode"`
	Database bool                  `json:"database"`
	Metered  bool                  `json:"metered"`
	Tenant   string                `json:"tenant"`
	APIKeys  []StatelessModeAPIKey `json:"api_keys"`
}

// StatelessModeAPIKey is one declared key as the mode report shows it.
type StatelessModeAPIKey struct {
	Name      string   `json:"name"`
	Roles     []string `json:"roles"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expires_at,omitempty"`
}

// NewStatelessMode builds the mode report from the declared keys.
func NewStatelessMode(keys []config.DeclaredAPIKey) StatelessMode {
	m := StatelessMode{
		Mode:    core.EngineModeStateless,
		Tenant:  core.DefaultTenantSlug,
		APIKeys: make([]StatelessModeAPIKey, 0, len(keys)),
	}
	for _, k := range keys {
		entry := StatelessModeAPIKey{Name: k.Name, Roles: nonNil(k.Roles), Scopes: nonNil(k.Scopes)}
		if !k.ExpiresAt.IsZero() {
			entry.ExpiresAt = k.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		m.APIKeys = append(m.APIKeys, entry)
	}
	return m
}

func onlyDefaultTenant(_ context.Context, slug string) bool {
	return slug == core.DefaultTenantSlug
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// NewStatelessRouter is the one HTTP surface of an engine running with no
// database. It serves the health probes, the routes of the plugins that run
// without one under /api/v1 and /api/admin, three engine routes an operator
// reads with a declared key holding an admin role (the mode, the plugin
// status and the entitlements), and the running plugins, which a key reads on
// the terms of any authenticated route.
//
// Callers authenticate only with a declared API key: there is no user table,
// so no login, no session and no cookie, which is also why no route here
// checks a CSRF token. Every request runs as the default tenant.
//
// The same guards as the API router run ahead of everything: security
// headers, the client address, path sanitizing, the body ceiling, the per-IP
// limiter, CORS and the content-type check.
func NewStatelessRouter(cfg *config.Config, mode StatelessMode, opts ...RouterOption) http.Handler {
	o := applyRouterOptions(opts)

	r := chi.NewRouter()
	applyAccessLogging(r, "cms-api")
	applyBuiltinMiddleware(r, cfg, pluginBodyOverrides(o.declaredRouteSets()),
		apimw.APISecurityHeaders(cfg.SecureCookie),
		apimw.StripUntrustedProxyHeaders(o.trustedProxies),
		apimw.ClientAddress(o.trustedProxies, cfg.ConsoleKeys()...))
	// A path a flow claims is answered here, where the API router answers it,
	// so a relay's inbound URL is the same in either mode.
	applyCustomRoutes(r, o.customRoutes)
	r.Use(middleware.Recoverer)
	r.Use(apimw.AllowedHosts(cfg.AllowedHosts))
	r.Use(apimw.HTTPSRedirect(cfg.SecureCookie))
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
	for _, mw := range o.preAuth {
		r.Use(mw)
	}
	// An admin token is for the admin API. Here it is refused outright,
	// on every route, rather than read as no credential.
	r.Use(refuseAdminTokens)

	if o.healthProbes != nil {
		r.Get("/healthz", healthzHandler(o.healthProbes))
		r.Get("/readyz", readyzHandler(o.healthProbes))
		r.Get("/startup", startupHandler(o.healthProbes))
	}

	publicRL := buildPublicRateLimiter(o)
	// The chain every mounted prefix shares: the declared key, then the one
	// tenant, then the caller's middleware (the license first among them) and
	// the idempotency slot, in the order the API router mounts them.
	authed := func(r chi.Router) {
		if o.apiKeyAuth != nil {
			r.Use(o.apiKeyAuth)
		}
		// A scoped key is held to the scope its path names on every route,
		// public ones included: a declared key is the only credential here,
		// and its scopes are the operator's only way to narrow one. A
		// plugin route's declared scope is checked after this one, so here
		// it can only narrow a key and never widen what the path grants.
		r.Use(apimw.RequireScoped)
		// There is one tenant. A super_admin key naming another in
		// X-Tenant-ID is refused rather than handed a tenant nothing else
		// knows about.
		r.Use(apimw.TenantHeader(false, onlyDefaultTenant))
		for _, mw := range o.extra {
			r.Use(mw)
		}
		if o.idempotency != nil {
			r.Use(o.idempotency)
		}
	}

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(customRoutesFirst(r, o.customRoutes))
		authed(r)
		mountDeclaredRoutes(r, o.pluginRoutes, o.ownerRoutes, o.entitlements.Withholds, "/api/v1", false, cfg.MaxJSONBodyBytes, publicRL, nil, nil)
	})

	r.Route("/api/admin", func(r chi.Router) {
		authed(r)
		mountDeclaredRoutes(r, o.pluginRoutes, o.ownerRoutes, o.entitlements.Withholds, "/api/admin", false, cfg.MaxJSONBodyBytes, publicRL, nil, nil)
		// Which plugins run, for any key the chain above admits: one holding
		// an admin role, or one whose scopes cover plugins:read.
		r.With(requireAuth).Get("/plugins/running", runningPluginsHandler(o.pluginStatus, o.entitlements))
		r.Group(func(r chi.Router) {
			r.Use(requireRole(core.RoleAdmin, core.RoleSuperAdmin))
			r.Get("/mode", func(w http.ResponseWriter, _ *http.Request) {
				httpx.JSON(w, http.StatusOK, mode)
			})
			r.Get("/plugins/status", pluginsStatusHandler(o.pluginStatus))
			r.Get("/entitlements", entitlementsHandler(o.entitlements, o.licenseModule))
		})
	})

	return r
}
