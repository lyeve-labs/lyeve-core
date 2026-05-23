package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/metrics"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// statelessScaling is the goroutine machinery the boot built before it knew
// the mode. The stateless host carries it so a plugin gets the same pool and
// tracker either way.
type statelessScaling struct {
	workerPool *engine.WorkerPool
	tracker    *engine.GoroutineTracker
	parallel   *engine.ParallelEngine
	asyncHooks *engine.AsyncHookExecutor
}

// runStateless boots the engine with no database, the mode LYEVE_MODE=stateless
// asks for. It serves one listener, the API address, until ctx ends.
//
// What it keeps from the full boot: the license, the hook registry, the plugin
// activator and the request guards. The license is verified and renewed the
// same way. The activator is limited to plugins that declare they run
// without a database. What it has none of: migrations, users and login,
// tenants beyond the default one, the admin listener, the replica bus and
// metering. API keys come from the api_keys
// section of the configuration tree. Nothing it holds survives a restart.
func runStateless(ctx context.Context, cfg *config.Config, opts Options, blog *bootLog, scaling statelessScaling) error {
	logger := blog.logger

	keys, err := config.LoadDeclaredAPIKeys("")
	if err != nil {
		return fmt.Errorf("stateless mode: %w", err)
	}
	if len(keys) == 0 {
		logger.Warn("stateless mode: the configuration declares no api_keys, so only public routes answer")
	}
	if cfg.BackpressureEnabled {
		// Backpressure sheds load from the database pool's pressure, and there
		// is no pool. Left on, it would build a limiter reading nothing.
		logger.Warn("stateless mode: BACKPRESSURE_ENABLED is ignored; it measures a database pool this mode does not have")
		c := *cfg
		c.BackpressureEnabled = false
		cfg = &c
	}
	logger.Warn("stateless mode: no database. Nothing is metered, one tenant is served, and nothing this process holds survives a restart",
		"api_keys", len(keys), "api_addr", cfg.APIListenAddr)

	// License: the same credentials as the full boot, handed to the licensing
	// implementation with no database and no bus, because there is no
	// database to keep a renewal in and no replica to share one with.
	hookRegistry := hooks.NewRegistry()
	mgr, err := opts.licensingVerifier().NewManager(ctx, licensing.Env{
		Credential:    cfg.LicenseKey,
		CacheDir:      cfg.LicenseCacheDir,
		InstanceID:    cfg.InstanceID,
		ServerURL:     cfg.LicenseServerURL,
		EncryptionKey: cfg.EncryptionKey,
		Names:         core.CompiledFeatureNames(),
		Logger:        logger,
	})
	if err != nil {
		return fmt.Errorf("stateless mode: license load: %w", err)
	}
	mgr.OnChange(func(c licensing.Change) {
		if err := hookRegistry.RunSystem(context.Background(), "license.changed", map[string]any{
			"plan":     c.Plan,
			"features": c.Features,
		}); err != nil {
			logger.Warn("license.changed hook failed", "err", err)
		}
	})
	mgr.Start(ctx)

	metrics.Init()
	hostBase := enginehost.NewHost(db.NoDatabase(), nil, cfg, hookRegistry, opts.ServiceName)
	wireStatelessHost(hostBase, cfg, mgr, blog, scaling)

	activator := plugin.NewActivator(hostBase, logger)
	if setter, ok := hostBase.(interface{ WithFlowRegistry(core.FlowRegistry) }); ok {
		setter.WithFlowRegistry(activator.FlowRegistry())
	}
	if setter, ok := hostBase.(interface {
		WithTenantRegions(core.TenantRegionResolverProvider)
	}); ok {
		setter.WithTenantRegions(activator)
	}
	activator.RequireStateless()
	activator.Resolve(mgr, strings.Join(cfg.Plugins, ","))
	if err := activator.Start(ctx); err != nil {
		logger.Error("plugin activator start failed", "err", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = activator.Stop(stopCtx)
	}()
	if err := activator.CheckRoles(); err != nil {
		return fmt.Errorf("stateless mode: plugin roles: %w", err)
	}

	trustedCIDRs, err := reqparse.ParseCIDRs(cfg.TrustedProxies)
	if err != nil {
		logger.Warn("invalid TRUSTED_PROXIES value - X-Forwarded-For will not be trusted", "error", err)
		trustedCIDRs = nil
	}
	publicLimitOverrides, publicGlobalOverride, err := apimw.ParsePublicRateLimits(cfg.PublicRateLimits, cfg.PublicRateLimitGlobal)
	if err != nil {
		return fmt.Errorf("stateless mode: public rate limits: %w", err)
	}

	healthReg := api.NewProbeRegistry()
	healthReg.Add(api.NewPluginReadinessProbe(activator.PluginReadinessProbe()))

	var noBackpressure apimw.BackpressureResult
	extra := buildAPIExtraMW(ctx, cfg, activator, NewInflightDrainer(), noBackpressure, apimw.NewLatencyTracker(200, 500))

	routes := activator.CollectedRoutes()
	publishEngineRateLimits(hostBase, cfg, routes)
	routerOpts := append([]api.RouterOption{
		api.WithHealthProbes(healthReg),
		api.WithPluginStatus(activator),
		api.WithEntitlements(mgr),
		api.WithLicenseModule(!licensing.IsOpen(opts.Licensing)),
		api.WithPluginRoutes(routes),
		api.WithCustomRoutes(activator),
		api.WithAPIKeyAuth(apimw.APIKeyAuth(declaredKeyLookup(keys, time.Now))),
		api.WithTrustedProxies(trustedCIDRs),
		api.WithPublicRateLimitOverrides(publicLimitOverrides, publicGlobalOverride),
	}, extra...)
	statelessRouter := api.NewStatelessRouter(cfg, api.NewStatelessMode(keys), routerOpts...)
	if reg, ok := hostBase.(core.APIRoutesRegistrar); ok {
		reg.RegisterAPIRoutes(api.NewRouteIndex(func() http.Handler { return statelessRouter }))
	}
	handler := api.ReadinessGate(healthReg)(statelessRouter)

	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return fmt.Errorf("stateless mode: TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}
	listenerTLS, err := listenerTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return fmt.Errorf("stateless mode: %w", err)
	}
	srv := &http.Server{
		Addr:              cfg.APIListenAddr,
		Handler:           handler,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig:         listenerTLS,
	}
	return serveUntilDone(ctx, "stateless mode", []*http.Server{srv}, logger)
}

// wireStatelessHost hands the host what a plugin can use with no database:
// the license, the goroutine machinery, the log ring and the metrics
// registry. The engine's limits follow once the routes are collected.
func wireStatelessHost(hostBase core.Host, cfg *config.Config, mgr licensing.Manager, blog *bootLog, scaling statelessScaling) {
	if setter, ok := hostBase.(interface{ WithLicensing(licensing.Manager) }); ok {
		setter.WithLicensing(mgr)
	}
	if setter, ok := hostBase.(interface{ WithWorkerPool(*engine.WorkerPool) }); ok {
		setter.WithWorkerPool(scaling.workerPool)
	}
	if setter, ok := hostBase.(interface {
		WithGoroutineTracker(*engine.GoroutineTracker)
	}); ok {
		setter.WithGoroutineTracker(scaling.tracker)
	}
	if setter, ok := hostBase.(interface{ WithParallelEngine(*engine.ParallelEngine) }); ok {
		setter.WithParallelEngine(scaling.parallel)
	}
	if setter, ok := hostBase.(interface {
		WithAsyncHookExecutor(*engine.AsyncHookExecutor)
	}); ok {
		setter.WithAsyncHookExecutor(scaling.asyncHooks)
	}
	if setter, ok := hostBase.(interface{ WithLogRing(core.LogRing) }); ok {
		setter.WithLogRing(blog.LogRing())
	}
	if reg, ok := hostBase.(core.MetricsGathererRegistrar); ok {
		reg.RegisterMetricsGatherer(metrics.Registry())
	}
}

// declaredKeyLookup answers the API key middleware from the keys the
// configuration declares. The middleware hands it the SHA-256 of the presented
// key, which is the form the file holds. A key past its expires_at is refused
// the same way an unknown one is.
//
// No API key pepper is installed in this mode, so the middleware's hash and the
// file's are the same SHA-256.
func declaredKeyLookup(keys []config.DeclaredAPIKey, now func() time.Time) apimw.APIKeyLookupFn {
	byHash := make(map[string]config.DeclaredAPIKey, len(keys))
	for _, k := range keys {
		byHash[k.SHA256] = k
	}
	return func(_ context.Context, hash string) (*core.AuthClaims, error) {
		k, ok := byHash[hash]
		if !ok {
			return nil, nil
		}
		if !k.ExpiresAt.IsZero() && !now().Before(k.ExpiresAt) {
			slog.Info("stateless mode: an expired api key was presented", "api_key", k.Name)
			return nil, nil
		}
		id := declaredKeyID(k.Name).String()
		return &core.AuthClaims{
			UserID:   id,
			Email:    "apikey:" + k.Name,
			Roles:    append([]string(nil), k.Roles...),
			Scopes:   append([]string(nil), k.Scopes...),
			TenantID: core.DefaultTenantSlug,
			IsAPIKey: true,
			APIKeyID: id,
		}, nil
	}
}

// declaredKeyID is the id a declared key acts under. Callers expect a key's id
// to be a UUID, as a stored key's is, and the same name gives the same id on
// every replica and after every restart.
func declaredKeyID(name string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("lyeve:declared-api-key:"+name))
}
