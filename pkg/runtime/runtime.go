// Package runtime implements the LyEve CMS boot path
// (config->logging->DB->migrations->license->JWT->plugins->HTTP) shared by all
// CMS binaries. Plugins self-register via plugin.Activator. This package never
// imports plugin source directly.
package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/compliance"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/eventbus"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	poolpkg "github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/metrics"
	"github.com/lyeve-labs/lyeve-core/internal/provider"
	"github.com/lyeve-labs/lyeve-core/internal/storage"
	"github.com/lyeve-labs/lyeve-core/internal/tracing"

	"github.com/lyeve-labs/lyeve-core/pkg/cluster"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
	"github.com/lyeve-labs/lyeve-core/pkg/security/secrets"
)

// Options carry build-time identity injected via -ldflags from the binary's
// main package. All fields are optional. Sensible defaults are applied.
type Options struct {
	// Version is the semver tag (e.g. "v0.5.0"). Defaults to "dev".
	Version string

	// Commit is the short git SHA the binary was built from.
	Commit string

	// BuildDate is an RFC3339 timestamp of the build.
	BuildDate string

	// ServiceName is the service identifier reported to OTLP. Defaults to
	// "lyeve-core".
	ServiceName string

	// Licensing is the licensing implementation this binary verifies with.
	// Nil runs on licensing.Open, which verifies nothing and starts every
	// compiled plugin.
	//
	// It is taken here, at the top of the boot, because step 4 loads the
	// license and step 7 starts the plugins the license decides. Nothing
	// after this call can change it, and nothing outside the caller can set
	// it: which licenses a binary honors is a property of what was compiled
	// into it.
	Licensing licensing.Verifier

	// CapPolicy is the capability table the build grants its plugins: each
	// entry is the most that plugin's host lets it reach, and the plugin's
	// own declaration only narrows it. The engine carries no table of its
	// own, so nil grants each plugin what it declared through
	// RegisterPluginWithCaps, and nothing to a plugin that declared nothing.
	CapPolicy map[string]core.Capability

	// ControlOrder names security controls in the order the security
	// controls report lists them. The plugins that supply the controls
	// report them in plugin name order, so a build whose console reads the
	// rows in a fixed order names that order here. The rows of a control it
	// does not name follow, in the order the plugins report them. Nil keeps
	// the plugins' order.
	ControlOrder []string

	// SchemaVersionSeed reports the engine schema version a database already
	// holds when the engine's migration bookkeeping table is empty, so a
	// build can carry the version of an install that recorded it some other
	// way. It runs once, before the first migration, and an error stops the
	// boot. Nil starts an empty table at the first migration.
	SchemaVersionSeed func(ctx context.Context, db *sql.DB, engine string) (uint, bool, error)
}

// licensingVerifier is the licensing implementation this run uses. A build
// that links none runs on licensing.Open, because the engine holds no license
// format, key or policy of its own to fall back on.
func (o Options) licensingVerifier() licensing.Verifier {
	if o.Licensing != nil {
		return o.Licensing
	}
	return licensing.Open()
}

// Run executes the full LyEve CMS boot path with default Options.
func Run(ctx context.Context) error {
	return RunWithOptions(ctx, Options{})
}

// RunWithOptions executes the full LyEve CMS boot path:
//
//  1. Load config from the environment
//  2. Initialize structured logging + OTLP telemetry
//  3. Connect pgxpool, run migrations
//  4. Load the license through the licensing implementation and start it
//  5. Wire the event bus
//  6. Build the engine host with ContentStore wired
//  7. Resolve + Start the in-process plugin activator
//  8. Mount admin (:3001) and API (:3002) HTTP servers
//  9. Block on ctx.Done(), then graceful-stop everything
//
// Returns the first fatal boot error. HTTP server errors after boot are
// logged but never returned. The canonical shutdown signal is ctx.Done().
//
// Callers: a main package that blank-imports the plugins it links, so the
// activator sees a populated registry, and passes the licensing
// implementation it links in Options.Licensing.
func RunWithOptions(ctx context.Context, opts Options) error {
	runStarted := time.Now()
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.ServiceName == "" {
		opts.ServiceName = "lyeve-core"
	}
	// Every scoped host is built from the table in force, so the build's
	// table is installed before any plugin can start.
	plugin.SetCapPolicy(opts.CapPolicy)
	// The report reads the order on every request, so a caller that reuses
	// its slice after this call must not reorder a running engine's report.
	opts.ControlOrder = append([]string(nil), opts.ControlOrder...)

	cfg, err := config.Load()
	if req, ok := config.IsSetupRequired(err); ok {
		return runSetupMode(ctx, req, slog.Default())
	}
	if err != nil {
		slog.Error("failed to load config", "err", err)
		return fmt.Errorf("config load: %w", err)
	}

	// Derive the master KEK while the database connects. The derivation is
	// cached per key, so the first KeyStore a plugin builds finds it ready
	// instead of spending its start level on it.
	go func() { _, _ = encryption.NewKeyStore(cfg.EncryptionKey) }()

	// GOMEMLIMIT + GC tuning
	// Set a soft memory cap at 85% of the container limit so Go's GC triggers
	// before the OOM killer. GOMEMLIMIT env var takes precedence when set.
	if os.Getenv("GOMEMLIMIT") == "" && cfg.MemoryLimitBytes > 0 {
		limit := int64(float64(cfg.MemoryLimitBytes) * 0.85)
		debug.SetMemoryLimit(limit)
	}

	// GOMAXPROCS
	// The memory limit above is read from the cgroup, and the CPU allowance
	// has to be too, or the scheduler sizes itself for the host. Under a
	// one-core quota Go would build a P and a garbage collection worker for
	// every core the machine has, arrange that much parallel work, and leave
	// the kernel to throttle the excess. That surfaces as latency spikes with
	// nothing in the logs to attribute them to.
	//
	// GOMAXPROCS in the environment wins, the way GOMEMLIMIT does above.
	if os.Getenv("GOMAXPROCS") == "" && cfg.CPUQuota > 0 && cfg.CPUQuota < goruntime.NumCPU() {
		goruntime.GOMAXPROCS(cfg.CPUQuota)
		slog.Info("sized the scheduler to the cgroup cpu allowance",
			"gomaxprocs", cfg.CPUQuota, "host_cpus", goruntime.NumCPU())
	}

	// Goroutine Concurrency Engine
	// Initialize centralized worker pool, goroutine tracker, parallel engine,
	// and async hook executor. The configuration sets the boot values. A
	// plugin reading core.GoroutineTunablesProvider can change the pool size,
	// the parallel ceiling and the async hook queue while the process runs.
	// Those changes do not persist across a restart.
	//
	// The executor always has the pool, whether or not it dispatches to it,
	// so a runtime tunable can turn async hooks on without a restart. It
	// starts disabled unless both flags ask for it.
	wpCfg := engine.DefaultWorkerPoolConfig()
	if cfg.GoroutineEngineEnabled {
		wpCfg.Size = cfg.GoroutineEnginePoolSize
	}
	workerPool := engine.NewWorkerPool(wpCfg)
	tracker := engine.NewGoroutineTracker(engine.DefaultTrackerConfig())
	parallel := engine.NewParallelEngine(engine.ParallelConfig{
		MaxConcurrent: cfg.DBWarmupParallelism,
		Timeout:       30 * time.Second,
	})
	hookCfg := engine.DefaultAsyncHookConfig()
	if cfg.AsyncHookTimeout > 0 {
		hookCfg.Timeout = cfg.AsyncHookTimeout
	}
	asyncHooks := engine.NewAsyncHookExecutor(workerPool, hookCfg)
	if err := asyncHooks.SetEnabled(cfg.AsyncHooksEnabled && cfg.GoroutineEngineEnabled); err != nil {
		return fmt.Errorf("async hooks: %w", err)
	}

	// Structured logging + telemetry
	blog, err := setupLogging(ctx, cfg, opts)
	if err != nil {
		return err
	}
	logger := blog.logger
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = blog.traceShutdown(shutCtx)
	}()

	// Everything from here on needs a database, so an engine opted into
	// stateless mode leaves the full boot here.
	if cfg.Stateless() {
		return runStateless(ctx, cfg, opts, blog, statelessScaling{
			workerPool: workerPool,
			tracker:    tracker,
			parallel:   parallel,
			asyncHooks: asyncHooks,
		})
	}

	// Database
	poolOpts := db.ConnectOptions{
		MaxConns:          cfg.DatabaseMaxConns,
		MinConns:          cfg.DatabaseMinConns,
		ConnMaxLifetime:   cfg.DatabaseConnMaxLifetime,
		ConnMaxIdleTime:   cfg.DatabaseConnMaxIdleTime,
		HealthCheckPeriod: cfg.DatabaseHealthCheckPeriod,
	}
	// Tenant isolation is the TenancyConn middleware, which the router wires
	// after TenantHeader when cfg.MultiTenant is set.
	pool, err := db.ConnectWithOptions(context.Background(), cfg.DatabaseURL, poolOpts)
	if err != nil {
		logger.Error("failed to connect to database", "err", err)
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	// Slow query tracer
	// Wraps the pool so every QueryRow/Query/Exec is timed. The tracer
	// maintains a ring buffer of the latest slow queries per dialect,
	// exposed via the pool health endpoint for operational visibility.
	queryTracer := db.NewSlowQueryTracer(pool, 50)
	pool = queryTracer // pool now implements DB with slow-query tracking

	// Inject DB tracer so every QueryRow/Query/Exec/Begin gets an OTel span.
	if cfg.OTLPEndpoint != "" {
		if setter, ok := interface{}(pool).(interface{ SetDBTracer(*tracing.DBTracer) }); ok {
			setter.SetDBTracer(tracing.NewDBTracer(opts.ServiceName))
			logger.Info("db tracing enabled",
				"service", opts.ServiceName,
				"otlp_endpoint", cfg.OTLPEndpoint,
			)
		}
	}

	// Read replica
	// Connect a separate read-only pool when DATABASE_REPLICA_URL is set.
	// QuerierRO delegates reads (dashboard, schema listings) to this pool,
	// reducing load on the primary. When unset, QuerierRO reads from the
	// primary.
	if cfg.DatabaseReplicaURL != "" {
		if err := db.ConnectReplica(context.Background(), pool, cfg.DatabaseReplicaURL, cfg.DatabaseReplicaMaxConns); err != nil {
			logger.Warn("read replica unavailable - falling back to primary for all reads",
				"err", err,
				"replica_url", cfg.DatabaseReplicaURL,
			)
		} else {
			logger.Info("read replica connected",
				"replica_url", cfg.DatabaseReplicaURL,
				"max_conns", cfg.DatabaseReplicaMaxConns,
			)
		}
	}

	// Prepared statement cache
	stmtCache := db.NewStmtCache(100)
	defer stmtCache.Close()

	// Query result cache
	// TTL-based cache for read-heavy endpoints (schema listing, media,
	// content listing). 1000 entry cap, 30s default TTL. Plugins use
	// host.CachedFetch() to transparently cache their read results.
	queryCache := db.NewQueryCache(1000, 30*time.Second)

	// Connection pool warmup
	// Pre-open connections so the first wave of requests doesn't pay the
	// TCP+TLS+auth handshake cost. WarmupConnections fires n concurrent
	// pings against the pool to pre-fill to the configured MinConns.
	db.WarmupConnections(ctx, pool, int(poolOpts.MinConns))

	// Sonic JSON prewarm
	// Prime sonic's JIT compiler with common CMS response shapes so the
	// first real request doesn't stall on encoder compilation.
	poolpkg.PrewarmSonic()

	// Latency tracker
	// Per-endpoint p50/p95/p99 latency histograms for the /api/admin/debug/latency
	// endpoint. Tracks 200 endpoints with 500 samples each, ring-buffer style.
	latencyTracker := apimw.NewLatencyTracker(200, 500)

	// Backpressure / load-shedding
	// When BACKPRESSURE_ENABLED is set, every request passes through the
	// backpressure middleware before hitting rate limiters and handlers.
	// The middleware uses the live pool stats to shed load before the DB
	// pool is exhausted. Stop is called during graceful shutdown.
	var backpressureResult apimw.BackpressureResult
	if cfg.BackpressureEnabled {
		bp, err := apimw.Backpressure(apimw.BackpressureConfig{
			MaxInflight:           cfg.BackpressureMaxInflight,
			TenantQuotaPct:        cfg.BackpressureTenantQuotaPct,
			PoolStats:             poolStatsAdapter(pool.Stats),
			PoolPressureThreshold: cfg.BackpressurePoolPressureThreshold,
		})
		if err != nil {
			logger.Error("failed to create backpressure middleware", "err", err)
			return fmt.Errorf("backpressure init: %w", err)
		}
		backpressureResult = bp
		logger.Info("backpressure middleware enabled",
			"max_inflight", cfg.BackpressureMaxInflight,
			"tenant_quota_pct", cfg.BackpressureTenantQuotaPct,
			"pool_pressure_threshold", cfg.BackpressurePoolPressureThreshold,
		)
	}

	// Prometheus metrics
	metrics.Init()
	dbCollector := metrics.NewDBCollector(func() metrics.DBStats {
		stat := pool.Stats()
		return metrics.DBStats{
			MaxOpenConnections: stat.MaxOpenConnections,
			OpenConnections:    stat.OpenConnections,
			InUse:              stat.InUse,
			Idle:               stat.Idle,
			WaitCount:          stat.WaitCount,
			WaitDuration:       stat.WaitDuration.Seconds(),
			MaxIdleClosed:      stat.MaxIdleClosed,
			MaxIdleTimeClosed:  stat.MaxIdleTimeClosed,
			MaxLifetimeClosed:  stat.MaxLifetimeClosed,
		}
	})
	metrics.Registry().MustRegister(dbCollector)

	// Migrations (admin server owns DDL)
	if n, err := db.Migrate(cfg.DatabaseURL, cfg.MigrationsPath, opts.SchemaVersionSeed); err != nil {
		logger.Error("migration failed", "err", err)
		return fmt.Errorf("migrate: %w", err)
	} else if n > 0 {
		logger.Info("migrations applied", "count", n)
	}

	// SQL Server snapshot reads
	if ok, remedy, err := db.CheckSnapshotReads(ctx, pool); err != nil {
		logger.Warn("could not read the snapshot isolation setting", "err", err)
	} else if !ok {
		logger.Warn("SQL Server is serving reads with shared locks, so a read can be chosen as a deadlock victim under concurrent writes; run this in a maintenance window",
			"remedy", remedy)
	}

	// Hooks
	// Created ahead of the license, whose changes are published on it, and
	// ahead of the engine host, so subscribers see every lifecycle event from
	// the moment plugins start.
	hookRegistry := hooks.NewRegistry()

	// Instance bus
	// Replicas share the database and nothing else, so every cache this
	// process keeps in memory, and the license, would drift from the others
	// until a TTL or a restart. The relay carries those changes once a
	// transport is attached, which a plugin providing
	// core.ClusterTransportProvider supplies after it starts. It exists
	// before the license, which the licensing implementation shares on it,
	// and every subscription is taken during boot, so an install that never
	// gets a transport runs single-instance and drops what it broadcasts.
	// See pkg/cluster.
	replicas := newReplicaSync(cfg.InstanceID, logger)

	// The engine's own *sql.DB, which plugins reach through RawDB and the
	// licensing implementation keeps its state in.
	var rawDB *sql.DB
	if r, ok := interface{}(pool).(interface{ SQLDB() *sql.DB }); ok {
		rawDB = r.SQLDB()
	}

	// License
	// The licensing implementation loads the license once, before any plugin
	// starts, from what the engine hands it. What LYEVE_LICENSE_KEY holds,
	// where a renewal is kept and which plugins a license starts are its to
	// decide. A missing or invalid license is an install entitled to less,
	// and only a broken implementation stops the boot.
	licEnv := licensing.Env{
		Credential:    cfg.LicenseKey,
		CacheDir:      cfg.LicenseCacheDir,
		InstanceID:    cfg.InstanceID,
		ServerURL:     cfg.LicenseServerURL,
		EncryptionKey: cfg.EncryptionKey,
		Bus:           replicas.Bus(),
		Names:         core.CompiledFeatureNames(),
		Logger:        logger,
	}
	if rawDB != nil {
		licEnv.DB, licEnv.Dialect = rawDB, pool.Engine()
	}
	mgr, err := opts.licensingVerifier().NewManager(ctx, licEnv)
	if err != nil {
		logger.Error("license load failed", "err", err)
		return fmt.Errorf("license load: %w", err)
	}
	// Whether this build links a licensing implementation, which the
	// entitlements endpoint reports. It follows from what was compiled in,
	// never from the license the implementation loaded.
	licenseModule := !licensing.IsOpen(opts.Licensing)
	// Every gated plugin re-reads its gates on license.changed, so a change of
	// the license, taken here or reported by another replica, is published
	// there.
	mgr.OnChange(func(c licensing.Change) {
		if err := hookRegistry.RunSystem(context.Background(), "license.changed", map[string]any{
			"plan":     c.Plan,
			"features": c.Features,
		}); err != nil {
			logger.Warn("license.changed hook failed", "err", err)
		}
	})
	mgr.Start(ctx)

	// JWT signing key
	// Initialize Ed25519 keypair for EdDSA JWT signing (or fall back to
	// HMAC-SHA256 if JWT_ALG=HS256 or the key file cannot be created).
	if err := auth.InitJWTSigning(cfg.JWTKeyPath); err != nil {
		if cfg.IsProduction() {
			logger.Error("jwt signing init failed in production - aborting boot", "err", err)
			return fmt.Errorf("jwt signing init in production: %w", err)
		}
		logger.Warn("jwt signing init failed, falling back to HS256 (dev only)", "err", err)
	}

	// Wire Ed25519 public key so security.ParseJWT can validate EdDSA tokens.
	// Plugins use security.ParseJWT (not auth.Parse) and need the key.
	security.SetEd25519PublicKey(auth.PublicKey())

	// The licensing implementation is never handed the instance's JWKS key.

	// Refresh token store
	// Refresh token rotation with reuse detection is backed by a plugin's
	// shared cache backend when one is running, or a built-in in-process
	// backend otherwise. The kernel dials no cache server itself. The store is
	// built after the activator starts, because the backend's plugin has to be
	// up first.
	var refreshTokenStore *auth.RefreshTokenStore
	var refreshBackendDistributed bool

	// Event bus
	// Content events reach the configured publisher through a hook on the
	// registry, wired before the engine host for the same reason.
	busPublisher, err := eventbus.NewFromEnv()
	if err != nil {
		logger.Error("failed to initialize event bus", "err", err)
		return fmt.Errorf("bus init: %w", err)
	}
	defer busPublisher.Close()

	busHook := hooks.NewBusHook(busPublisher)
	for _, et := range []hooks.EventType{hooks.AfterCreate, hooks.AfterUpdate, hooks.AfterDelete} {
		hookRegistry.Register("*", et, busHook)
	}

	// Engine host
	// The engine host is the single concrete core.Host backing every
	// in-process plugin.
	hostBase := enginehost.NewHost(pool, rawDB, cfg, hookRegistry, opts.ServiceName)
	if rawDB != nil {
		attachSessionPools(context.Background(), hostBase, pool.Engine(), cfg.DatabaseURL, logger)
	}
	// Wire the query cache so plugins can use host.CachedFetch() for
	// transparent read-result caching.
	if setter, ok := hostBase.(interface{ WithQueryCache(*db.QueryCache) }); ok {
		setter.WithQueryCache(queryCache)
	}

	// A plugin's cache invalidation leaves on the relay, and the ones the
	// other replicas send arrive here.
	if setter, ok := hostBase.(interface{ WithClusterBus(cluster.Bus) }); ok {
		setter.WithClusterBus(replicas.Bus())
	}
	replicas.wireQueryCache(hostBase)

	// Capabilities and HasFeature answer from the license the manager loaded.
	if setter, ok := hostBase.(interface{ WithLicensing(licensing.Manager) }); ok {
		setter.WithLicensing(mgr)
	}

	// Pool health
	// Start background pool health monitoring for the admin dashboard.
	// This runs regardless of whether an external pooler (PgBouncer/ProxySQL)
	// is configured: it monitors the internal *sql.DB pool either way.
	healthMaxLatency, healthMinIdle, healthMaxUtil := config.LoadPoolHealthOptions()
	poolMonitor := db.NewPoolHealthMonitor(db.PoolMonitorConfig{
		Pool:          pool,
		PoolSizeCfg:   cfg.PoolSizing,
		StmtCache:     stmtCache,
		MaxLatency:    healthMaxLatency,
		MinIdle:       healthMinIdle,
		MaxUtil:       healthMaxUtil,
		CheckInterval: 30 * time.Second,
	})
	poolMonitor.Start(ctx)
	defer poolMonitor.Stop()

	// Wire pool health into the engine host so /api/admin/pool/health works.
	if setter, ok := hostBase.(interface {
		WithPoolHealth(*db.PoolConfig, *db.StmtCache, time.Duration, int, float64)
	}); ok {
		setter.WithPoolHealth(cfg.PoolSizing, stmtCache, healthMaxLatency, healthMinIdle, healthMaxUtil)
	}
	if setter, ok := hostBase.(interface {
		WithPoolHealthMonitor(*db.PoolHealthMonitor)
	}); ok {
		setter.WithPoolHealthMonitor(poolMonitor)
	}

	// Database provider capabilities
	// Bridge the db/providers registry into the unified provider registry so
	// auto-detection and health-check-all work across all provider categories.
	provider.RegisterDBProviders()

	// Cache provider
	// External cache backends come from a plugin. The kernel holds no cache
	// client of its own.

	// Storage provider
	// When STORAGE_DRIVER is set to an external provider (s3 or minio),
	// connect through the unified provider framework. The host's local
	// storage (set below) remains as the fallback. The connected provider
	// is available via StorageConnectedProvider for plugins that need direct
	// blob access.
	if cfg.StorageDriver != "" && cfg.StorageDriver != "local" {
		_ = resolveAndWireStorageProvider(hostBase, cfg)
	}

	// Storage backend
	// Default to local filesystem storage. A plugin implementing StorageSetter
	// can replace it on activation.
	storageBackend, _ := storage.NewLocal(cfg.StorageLocalPath, cfg.StorageBaseURL)
	if setter, ok := hostBase.(interface{ WithStorage(core.Storage) }); ok {
		setter.WithStorage(storageBackend)
	}

	// Wire scaling engine into host
	if setter, ok := hostBase.(interface{ WithWorkerPool(*engine.WorkerPool) }); ok {
		setter.WithWorkerPool(workerPool)
	}
	if setter, ok := hostBase.(interface {
		WithGoroutineTracker(*engine.GoroutineTracker)
	}); ok {
		setter.WithGoroutineTracker(tracker)
	}
	if setter, ok := hostBase.(interface{ WithParallelEngine(*engine.ParallelEngine) }); ok {
		setter.WithParallelEngine(parallel)
	}
	if setter, ok := hostBase.(interface {
		WithAsyncHookExecutor(*engine.AsyncHookExecutor)
	}); ok {
		setter.WithAsyncHookExecutor(asyncHooks)
	}
	// The ring the slog chain has fed since boot, for a plugin reading
	// core.LogRingProvider.
	if setter, ok := hostBase.(interface{ WithLogRing(core.LogRing) }); ok {
		setter.WithLogRing(blog.LogRing())
	}
	// In-process plugin activator
	// Plugins linked into this binary (via blank imports in the binary's
	// main package) self-register from init(). The activator starts the
	// plugins that are compiled in, allowed by the capability set, and
	// requested by LYEVE_PLUGINS.

	// Migration signature cache
	// Initialize the migration signature cache so plugins skip the applied-
	// versions DB query when their migration files haven't changed since
	// the last boot. The cache is process-wide: PluginMigrate consults it
	// internally.
	if rawDB != nil {
		sigCache := plugin.NewMigrationSigCache(rawDB)
		if err := sigCache.Init(ctx, pool.Engine()); err != nil {
			logger.Warn("migration signature cache init failed - continuing without lazy migration checks",
				"err", err)
		} else {
			plugin.SetMigrationSigCache(sigCache)
			logger.Info("migration signature cache initialized",
				"dialect", pool.Engine())
		}
	}

	// Operator-set plugin configuration
	// Loaded before the activator runs: plugins read their configuration in
	// Start, so anything installed after this point would be missed. The
	// environment still wins: stored values only fill keys it leaves unset.
	if pool != nil {
		loadPluginConfigOverlay(ctx,
			db.NewPluginConfigStore(pool).WithSealer(db.NewConfigSealer(cfg.EncryptionKey)), logger)
	}

	// The account record is engine-owned, so no plugin exporter covers it and a
	// DSAR bundle would report the subject as unknown while sys_users still
	// holds their row. The eraser is registered for the same reason: without it
	// a DSAR erasure wipes plugin data but leaves the sys_users account row
	// behind.
	registerSysUserExporter(hostBase)
	registerSysUserEraser(hostBase)

	// The registry is the one /api/admin/metrics serves. The pool collector
	// was registered on it above, so an exporter plugin gathers every
	// engine series from its first tick.
	if reg, ok := hostBase.(core.MetricsGathererRegistrar); ok {
		reg.RegisterMetricsGatherer(metrics.Registry())
	}

	activator := plugin.NewActivator(hostBase, logger)
	// The flow registry is handed to the host before any plugin starts, so
	// the plugin that runs flows finds it in Start. The activator fills it
	// once every plugin is up and tells the subscribers.
	if setter, ok := hostBase.(interface {
		WithFlowRegistry(core.FlowRegistry)
	}); ok {
		setter.WithFlowRegistry(activator.FlowRegistry())
	}
	// The host answers a tenant's region from whichever running plugin holds
	// the role, asking the activator on each call.
	if setter, ok := hostBase.(interface {
		WithTenantRegions(core.TenantRegionResolverProvider)
	}); ok {
		setter.WithTenantRegions(activator)
	}
	// cfg.Plugins comes from LYEVE_PLUGINS.
	activator.Resolve(mgr, strings.Join(cfg.Plugins, ","))
	if err := activator.Start(ctx); err != nil {
		// Non-fatal: the engine continues serving even if every plugin fails.
		// /api/admin/plugins/status reports the failures so operators see them.
		logger.Error("plugin activator start failed", "err", err)
	}

	// Plugins are now started. If boot fails before we reach the graceful
	// shutdown path below, stop them here so their workers/resources don't
	// leak. Cleared just before the normal shutdown Stop so it runs exactly once.
	activatorNeedsStop := true
	defer func() {
		if activatorNeedsStop {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = activator.Stop(stopCtx)
		}
	}()

	// A role one plugin holds, which two running plugins implement, could be
	// wired from either of them, so the boot stops rather than pick one.
	if err := activator.CheckRoles(); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	}

	// Plugin capability audit
	// After activation, log each compiled plugin's capability grant, which is
	// the set its scoped host enforces. A plugin scoped by neither the host
	// policy nor RegisterPluginWithCaps runs with no capabilities at all,
	// because CapPolicy fails closed. It is logged as "none" and warned about.
	// A denied host method hands back a zero value or a stub, so without the
	// warning the plugin would look as if it runs.
	capsAudit := core.AllPluginCaps()
	var unScoped []string
	for _, e := range capsAudit {
		if e.Explicit {
			logger.Info("plugin registered with explicit capabilities",
				"plugin", e.Name,
				"caps", e.Caps,
			)
		} else {
			unScoped = append(unScoped, e.Name)
			logger.Warn("plugin registered without explicit capabilities runs with none: it must declare them with RegisterPluginWithCaps",
				"plugin", e.Name,
				"caps", "none",
			)
		}
	}

	// Strict mode: fail boot when any plugin lacks explicit caps.
	if cfg.PluginCapsStrictMode && len(unScoped) > 0 {
		return fmt.Errorf("PLUGIN_CAPS_STRICT_MODE is enabled but %d plugin(s) are registered without explicit capabilities: %s",
			len(unScoped), strings.Join(unScoped, ", "))
	}

	// Authorization is a contract the kernel ships a default for rather than
	// an implementation. Say which one is in force, because the difference is
	// whether a rule written by an operator can exist at all, and an install
	// that silently has no rule engine is the shape this is meant to prevent.
	if pcp, ok := hostBase.(core.PermissionCheckerProvider); ok {
		if _, roleOnly := pcp.PermissionChecker().(core.RoleOnlyPermissionChecker); roleOnly {
			logger.Info("no plugin supplies a permission checker, so authorization is roles only: a super admin is allowed everything, an admin is allowed the resources of kinds a plugin opens to admins, and every other role is refused")
		}
	}

	// Collect HTTP route declarations from plugins implementing RoutesPlugin.
	pluginRoutes := activator.CollectedRoutes()

	// Each router filters the set by its own prefix, so neither can tell a
	// route that belongs to the other from one that belongs to nobody. Say so
	// here, where the whole set is visible.
	api.WarnUnmountableRoutes(pluginRoutes)

	// Content types declared in configuration
	// Applied after plugins start, so the plugin that owns content has created
	// its tables, and before the cache warms, so the first request sees the
	// final set.
	if err := reconcileDeclaredSchemas(ctx, hostBase.Schema(), "", logger); err != nil {
		return fmt.Errorf("declared content types: %w", err)
	}

	// Plugin cache warmup
	// After all plugins have started, schema cache is warm, and capability
	// wiring is complete, tell every CacheWarmer plugin to preload its
	// caches so the first real request doesn't pay the cold-cache penalty.
	warmCacheStart := time.Now()
	var warmed int
	var warmErr error
	if cfg.GoroutineEngineEnabled && parallel.MaxConcurrent() > 1 {
		warmed, warmErr = activator.WarmCacheAllParallel(ctx, parallel)
	} else {
		warmed, warmErr = activator.WarmCacheAll(ctx)
	}
	if warmErr != nil {
		logger.Warn("plugin cache warmup had errors - continuing", "err", warmErr)
	}
	logger.Info("plugin cache warmup complete",
		"elapsed_ms", time.Since(warmCacheStart).Milliseconds(),
		"warmed", warmed)

	// Replication is a plugin. It owns the store the replicas broadcast
	// through, so an install without it is the single instance the relay has
	// been behaving as since boot.
	if ctp, ok, err := plugin.Only[core.ClusterTransportProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		if transport := ctp.ClusterTransport(); transport != nil {
			replicas.attach(ctx, transport)
			logger.Info("cluster transport wired - a cache, license or tenant feature change on one replica now reaches the others", "instance", replicas.Bus().InstanceID())
		}
	}

	// The tenancy roles, each read from the running plugin that implements
	// it. The archived checker (core.ArchivedCheckerProvider) becomes the API
	// router's ReadOnlyArchived middleware, so a write to an archived tenant
	// is rejected with HTTP 423 Locked.
	var archivedChecker core.TenantArchivedFunc
	var tenantValidator core.TenantValidatorFunc
	var tenantRoster core.TenantRosterFunc
	var tenantSlugs core.TenantSlugsFunc
	var defaultTenant core.DefaultTenantFunc
	var memberships core.MembershipReader
	var adminTokens core.AdminTokenStore
	if acp, ok, err := plugin.Only[core.ArchivedCheckerProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		archivedChecker = acp.ArchivedChecker()
		logger.Info("tenant archived checker wired - writes to archived tenants will be rejected with 423")
	}
	if tvp, ok, err := plugin.Only[core.TenantValidatorProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		tenantValidator = tvp.TenantValidator()
		logger.Info("tenant validator wired - super_admin X-Tenant-ID overrides validated against the tenant roster")
	}
	if trp, ok, err := plugin.Only[core.TenantRosterProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		tenantRoster = trp.TenantRoster()
		logger.Info("tenant roster wired - a public route that resolves no tenant answers 404 once the install holds more than one")
	}
	if tsp, ok, err := plugin.Only[core.TenantSlugsProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		tenantSlugs = tsp.TenantSlugs()
		logger.Info("tenant enumeration wired - a cross-tenant DSAR export can visit every tenant")
	}
	if dtp, ok, err := plugin.Only[core.DefaultTenantProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		defaultTenant = dtp.DefaultTenant()
		logger.Info("default tenant provisioning wired - first-run setup gives the bootstrap account a tenant")
	}
	if mp, ok, err := plugin.Only[core.MembershipProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		if reader := mp.MembershipReader(); reader != nil {
			memberships = reader
			logger.Info("cross-tenant membership wired - a session may act in a tenant other than the account's home one")
		}
	}
	// The admin seat guard plugins read from the host counts the seats a
	// membership grants, as the routers' guard does.
	if hb, ok := hostBase.(interface{ SetAdminSeatMembers(core.MembershipReader) }); ok {
		hb.SetAdminSeatMembers(memberships)
	}
	if atp, ok, err := plugin.Only[core.AdminTokenStoreProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		if store := atp.AdminTokenStore(); store != nil {
			adminTokens = store
			logger.Info("admin token storage wired - the admin API accepts admin tokens and serves the routes that issue them")
		}
	}

	// Tenant deletion has to reach every tenant-scoped table, whichever
	// plugins run. Every compiled plugin contributes the purge for its own
	// tables, started or not, the engine registers the handlers for the
	// tables no plugin owns, and it checks the coverage against the database
	// itself.
	if n := activator.RegisterTenantPurges(hostBase.Dialect()); n > 0 {
		logger.Info("plugin tenant purges registered", "plugins", n)
	}
	registerKernelPurgeHandlers(logger)
	if err := checkPurgeCoverage(ctx, hostBase.Querier(ctx), hostBase.Dialect(), logger); err != nil {
		return err
	}

	// Refresh token store backend
	// Ask the running plugins for an isolated auth backend
	// (core.AuthBackendProvider), then for a shared cache backend
	// (core.CacheBackendProvider). With neither, fall back to an in-process
	// backend so refresh-token rotation still works on a single instance. The
	// kernel holds no cache client either way.
	var refreshBackend core.CacheBackend
	// Prefer AuthBackend: an isolated instance that an admin cache flush
	// cannot reach, so refresh tokens survive the flush.
	if abp, ok, err := plugin.Only[core.AuthBackendProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok {
		if backend := abp.AuthBackend(); backend != nil {
			refreshBackend = backend
			refreshBackendDistributed = true
		}
	}
	// Fall back to the shared CacheBackend when no plugin offers an isolated
	// auth backend.
	if cbp, ok, err := plugin.Only[core.CacheBackendProvider](activator); err != nil {
		return fmt.Errorf("plugin roles: %w", err)
	} else if ok && refreshBackend == nil {
		if backend := cbp.CacheBackend(); backend != nil {
			refreshBackend = backend
			refreshBackendDistributed = true
		}
	}
	// A backend that declares itself process-local gets no remote probe, so
	// /readyz lists no "redis" probe on a host with no Redis. A backend that
	// does not declare keeps the probe.
	if d, ok := refreshBackend.(interface{ IsDistributed() bool }); ok && !d.IsDistributed() {
		refreshBackendDistributed = false
	}
	// Wrap the selected backend so Flush is a no-op for the refresh-token
	// store: if the plugin only exposes CacheBackend(), this still stops an
	// admin cache flush from wiping all refresh tokens.
	if refreshBackend != nil {
		refreshBackend = auth.NewFlushProtectedBackend(refreshBackend)
	}
	// Lockout and brute-force state
	// MFA lockouts and login blocks are counted in memory by the plugins that
	// own them, which means they are lifted by a restart and counted per
	// replica. Hand those plugins the same isolated backend the refresh-token
	// store uses so the state survives both, and say when it cannot: an
	// attempt limit that silently resets is worse than one that is documented
	// as process-local.
	if wired := activator.WireAuthCache(refreshBackend); wired > 0 {
		if refreshBackendDistributed {
			logger.Info("lockout state: shared cache backend, survives restart and is counted across replicas",
				"plugins", wired)
		} else {
			logger.Info("lockout state: process-local cache backend, survives restart but each replica counts its own attempts",
				"plugins", wired)
		}
	} else {
		logger.Info("lockout state: in-process only, cleared on restart and counted per replica (no cache backend is registered to persist it)")
	}

	// The engine's own login and MFA lockouts. Only a backend every replica
	// shares is worth moving them into. A process-local one would count per
	// replica exactly as the in-process maps do.
	var lockoutBackend core.CacheBackend
	if refreshBackend != nil && refreshBackendDistributed {
		lockoutBackend = refreshBackend
		logger.Info("login and mfa lockouts: shared cache backend, counted across replicas and kept across restarts")
	} else {
		logger.Info("login and mfa lockouts: in-process, cleared on restart and counted per replica (sharing them needs a shared cache backend such as Redis)")
	}

	// Whether sessions are shared decides whether this install can run more than
	// one replica. A refresh routed to a replica that does not hold the family
	// finds nothing and answers 401, which a client reads as "your session is
	// gone", so scaling a process-local store logs every user out at random.
	// Say so at boot: the symptom gives no hint of the cause.
	switch {
	case refreshBackend == nil:
		refreshBackend = auth.NewMemoryBackend()
		logger.Info("refresh token store: in-process, sessions are not shared between replicas (scaling out needs a shared cache backend such as Redis)",
			"ttl_secs", cfg.RefreshTokenTTL)
	case refreshBackendDistributed:
		logger.Info("refresh token store: shared cache backend, sessions are shared between replicas",
			"ttl_secs", cfg.RefreshTokenTTL)
	default:
		logger.Info("refresh token store: the registered cache backend is process-local, sessions are not shared between replicas (scaling out needs a shared backend such as Redis)",
			"ttl_secs", cfg.RefreshTokenTTL)
	}
	refreshTokenStore = auth.NewRefreshTokenStore(refreshBackend, "cms")

	// Subject erasers
	// Every compiled plugin declares the erasure for its own tables, started
	// or not, because a plugin that does not start in this run still holds
	// the people an earlier run wrote. A running plugin that exposes an eraser
	// through the provider interface is otherwise never reached by an
	// erasure request, which then reports success and a row count that
	// quietly excludes it.
	if n := activator.RegisterSubjectErasures(); n > 0 {
		logger.Info("plugin subject erasures registered", "plugins", n)
	}
	if n := activator.RegisterSubjectErasers(); n > 0 {
		logger.Info("subject erasers registered from the provider interface", "plugins", n)
	}
	logSubjectEraserRoster(ctx, logger)

	// Subject exporters, for the same reason: a plugin that exposes its
	// exporter rather than registering it would otherwise be missing from a
	// DSAR bundle that still reads as complete.
	if n := activator.RegisterSubjectExporters(); n > 0 {
		logger.Info("subject exporters registered from the provider interface", "plugins", n)
	}

	// Custom schema validators
	// A name no plugin claims falls back to the pattern interpreter, so this
	// is logged only when a plugin actually gave a name its own meaning.
	if n := activator.RegisterCustomValidators(); n > 0 {
		logger.Info("custom schema validators registered from plugins", "validators", n)
	}

	// Legal holds
	// Erasure consults this before the fan-out. With no checker registered no
	// hold can exist, and erasure proceeds.
	// The line is logged either way because "held subject erased anyway" and
	// "no holds are possible here" must not look alike after the fact.
	if activator.RegisterHoldChecker() {
		logger.Info("legal hold checker registered; erasure will skip held subjects")
	} else {
		logger.Info("no legal hold checker is active; erasure cannot be blocked by a hold")
	}

	// Wire refresh-token revocation into the host so security-sensitive plugins
	// (the users API on a password change, an eraser) can revoke all of a user's sessions.
	if hb, ok := hostBase.(interface {
		SetRefreshTokenRevoker(func(ctx context.Context, userID string) error)
	}); ok {
		hb.SetRefreshTokenRevoker(refreshTokenStore.RevokeAllForUser)
	}

	// Dev-mode hot-reload
	// When built with -tags dev, wireHotReload starts a fsnotify watcher
	// that rebuilds plugin .so files and swaps them at runtime.
	// On route-table changes the callback below rebuilds both routers
	// via SwappableHandler so updated plugin routes take effect without
	// dropping in-flight requests.
	if err := wireHotReload(ctx, activator); err != nil {
		logger.Warn("hot-reload wiring failed - continuing without it", "err", err)
	}

	// In-flight request drainer
	// Tracks active HTTP requests so the shutdown sequence can wait for them
	// to complete before stopping plugins and closing database connections.
	drainer := NewInflightDrainer()

	// Kubernetes health probes
	healthReg := api.NewProbeRegistry()
	// Database probe: liveness is driven by this.
	dbPing := func(ctx context.Context) error {
		if err := pool.Ping(ctx); err != nil {
			// Keep driver/connection detail server-side; /readyz gets a safe message.
			logger.Warn("readiness database ping failed", "err", err)
			return errors.New("database unavailable")
		}
		return nil
	}
	healthReg.Add(api.NewDBProbe(dbPing))
	// Cache-backend probe: only when refresh tokens use a distributed plugin
	// backend. The in-process fallback is always healthy, so no probe.
	if refreshTokenStore != nil && refreshBackendDistributed {
		redisRequired := os.Getenv("HEALTH_REDIS_PROBE_REQUIRED") == "true"
		healthReg.Add(api.NewRedisProbe(refreshTokenStore.Ping, redisRequired))
		if redisRequired {
			logger.Info("readiness: cache outage will take this pod out of service",
				"probe", "redis", "required", true)
		}
	}
	// Disk probe: check available space (configurable via env).
	diskPath := os.Getenv("HEALTH_DISK_PATH")
	if diskPath == "" {
		diskPath = cfg.StorageLocalPath // default from STORAGE_LOCAL_PATH, ./uploads
	}
	diskMinBytes := uint64(100 * 1024 * 1024) // 100 MiB
	if v := os.Getenv("HEALTH_DISK_MIN_MB"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			diskMinBytes = n * 1024 * 1024
		}
	}
	healthReg.Add(api.NewDiskProbe(diskPath, diskMinBytes))
	// Goroutine probe: advisory warning when count exceeds threshold.
	goroutineMax := 10000
	if v := os.Getenv("HEALTH_GOROUTINE_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			goroutineMax = n
		}
	}
	healthReg.Add(api.NewGoroutineProbe(goroutineMax))
	// Pool utilization probe (Required): removes pod from service when pool
	// is near exhaustion (default 80%). Threshold matches metrics.ExhaustionThreshold.
	poolUtilThreshold := 0.8
	if v := os.Getenv("POOL_EXHAUSTION_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1.0 {
			poolUtilThreshold = f
		}
	}
	healthReg.Add(api.NewPoolUtilizationProbe(func() (int, int) {
		stat := pool.Stats()
		return stat.InUse, stat.MaxOpenConnections
	}, poolUtilThreshold))
	// Plugin readiness probe: reports when all running plugins signal ready.
	healthReg.Add(api.NewPluginReadinessProbe(activator.PluginReadinessProbe()))
	// Provider health probes: one per connected external provider (storage
	// is the only one). Each is optional: skipped when the provider is not
	// configured or failed to connect.
	addProviderHealthProbes(healthReg, hostBase)

	// PII log handler
	// Wrap slog.Default() with the PII-masking log handler a plugin
	// contributes, when one is active. This sanitizes all log output (boot,
	// plugin, request) before hitting stdout/stderr.
	if piiLogHandler := activator.PIILogHandler(); piiLogHandler != nil {
		piiLogger := slog.New(piiLogHandler)
		slog.SetDefault(piiLogger)
		logger.Info("slog default handler wrapped with PII masking")
	}

	// Admin server (:3001)
	adminExtraMW := buildAdminExtraMW(ctx, cfg, activator, drainer, backpressureResult, latencyTracker)

	// Secret custody: resolve master secrets through the chain
	// A KMS/Vault plugin source (when active) takes precedence over the
	// process environment, so neither the API-key pepper nor the data-at-rest
	// master KEK need live in env vars. Missing values fall back to the
	// defaults: plain-SHA-256 API-key hashing, and the router deriving its
	// KEK from cfg.EncryptionKey (ENCRYPTION_KEY).
	secretSource := secrets.Chain{activator.SecretSource(), secrets.EnvSource{}}

	// API-key pepper (defense-in-depth for sys_api_keys). When present,
	// API-key hashes become HMAC-SHA256-keyed so a DB-only leak can't validate
	// or forge keys offline.
	if pepper, perr := secretSource.Secret(ctx, secrets.APIKeyPepper); perr == nil {
		security.SetAPIKeyPepper(pepper)
		slog.Info("API-key pepper installed from secret source - hashes are HMAC-keyed")
	} else if !errors.Is(perr, secrets.ErrNotFound) {
		slog.Warn("failed to resolve API-key pepper; continuing with plain-SHA-256 hashing", "error", perr)
	} else if cfg.EnforceAPIKeyPepper {
		// Defense-in-depth: refuse to start when the pepper is mandatory.
		// Without it, API-key hashes are plain SHA-256 and a DB-only leak
		// can validate keys offline.
		return fmt.Errorf("ENFORCE_API_KEY_PEPPER is set but API_KEY_PEPPER was not resolved from any secret source - refusing to start with plain-SHA-256 API-key hashing")
	}

	// Parse trusted proxy CIDRs for ClientIPTrusted in audit/auth paths.
	// When TRUSTED_PROXIES is empty, trustedCIDRs is nil -> ClientIPTrusted
	// falls back to RemoteAddr (spoof-proof, proxy-blind).
	trustedCIDRs, err := reqparse.ParseCIDRs(cfg.TrustedProxies)
	if err != nil {
		slog.Warn("invalid TRUSTED_PROXIES value - X-Forwarded-For will not be trusted for audit logs", "error", err)
		trustedCIDRs = nil
	}

	// Public endpoint caps. Empty settings yield no overrides, and the routers
	// then run the engine's shipped table. A malformed one stops the boot: a
	// limit an operator believes they set and the engine quietly ignored is
	// worse than a refusal to start.
	publicLimitOverrides, publicGlobalOverride, err := apimw.ParsePublicRateLimits(cfg.PublicRateLimits, cfg.PublicRateLimitGlobal)
	if err != nil {
		return fmt.Errorf("public rate limits: %w", err)
	}
	if len(publicLimitOverrides) > 0 || publicGlobalOverride != nil {
		slog.Info("public rate limits overridden",
			"routes", len(publicLimitOverrides),
			"global_set", publicGlobalOverride != nil)
	}

	// Master KEK for data-at-rest encryption (TOTP/OAuth secrets, per-tenant
	// DEKs). When the chain supplies it, build the KeyStore here and inject it
	// into every router. Otherwise the router falls back to cfg.EncryptionKey.
	var encKeyStore *encryption.KeyStore
	if kek, kerr := secretSource.Secret(ctx, secrets.EncryptionKEK); kerr == nil {
		if ks, berr := encryption.NewKeyStore(string(kek)); berr == nil {
			encKeyStore = ks
			slog.Info("data-at-rest KEK resolved from secret source")
		} else {
			slog.Warn("failed to build KeyStore from resolved KEK; falling back to ENCRYPTION_KEY", "error", berr)
		}
	} else if !errors.Is(kerr, secrets.ErrNotFound) {
		slog.Warn("failed to resolve data-at-rest KEK; falling back to ENCRYPTION_KEY", "error", kerr)
	}

	// The routes the licensing implementation serves, for one router build.
	// It is told first which plugins serve routes, because that is what a
	// tenant can be refused, and it changes whenever the routes are
	// collected again.
	licenseRoutes := func(routes []plugin.PluginRoutes) []plugin.PluginRoutes {
		if so, ok := mgr.(licensing.StartObserver); ok {
			names := make([]string, 0, len(routes))
			for _, pr := range routes {
				names = append(names, pr.Name)
			}
			so.PluginsStarted(names)
		}
		rp, ok := mgr.(licensing.RouteProvider)
		if !ok {
			return nil
		}
		return []plugin.PluginRoutes{{Name: rp.RouteOwner(), Routes: rp.Routes()}}
	}

	// Wire API key auth lookup when a plugin provides one. Both the admin and
	// API routers authenticate X-API-Key, so a scoped key is a first-class
	// caller on either port. The upgrade fn (may be nil) rewrites a
	// plain-SHA-256 hash to the peppered form on first use.
	var apiKeyLookupMW func(http.Handler) http.Handler
	if lookupFn := activator.APIKeyLookupFn(); lookupFn != nil {
		apiKeyLookupMW = apimw.APIKeyAuthWithUpgrade(lookupFn, activator.APIKeyUpgradeFn())
	}

	// Pre-auth middleware
	// Both routers mount this slot in one fixed order:
	//
	//	CORS -> request logging -> plugin pre-auth -> JWT auth -> TenantHeader
	//
	// Request logging stays first so a request a plugin turns away is still
	// logged. Plugin middleware follows, and sits above auth and tenancy
	// because that is what the slot is for: a plugin that resolves a tenant
	// from the request itself, by mapping a custom domain in the Host header
	// onto a slug, puts the answer on the context with core.WithTenantID, and
	// TenantHeader reads the context only after its header and claim branches
	// come up empty. Anything mounted below TenantHeader runs after it has
	// already decided, which on a multi-tenant install means an anonymous
	// request to a public route names no tenant and the store refuses it.
	//
	// Called per router build rather than captured once, so a plugin started
	// on demand after boot contributes when routes are next rebuilt.
	preAuthMW := func() []func(http.Handler) http.Handler {
		return append(
			[]func(http.Handler) http.Handler{logging.Middleware(blog.EnrichHandler())},
			activator.PreAuthMiddleware()...,
		)
	}

	// Every router build starts the schema cache's invalidation loop, which
	// polls the registry until its context ends. The hot reload replaces both
	// routers on every plugin route change, so without a lifetime per build
	// the process would accumulate one poller per rebuild, each polling every
	// second for as long as it runs. newRouterLifetime ends the pollers of the
	// build it replaces.
	var routerLifetimeMu sync.Mutex
	var cancelRouterLifetime context.CancelFunc
	newRouterLifetime := func() context.Context {
		routerLifetimeMu.Lock()
		defer routerLifetimeMu.Unlock()
		if cancelRouterLifetime != nil {
			cancelRouterLifetime()
		}
		lifetime, cancel := context.WithCancel(ctx)
		cancelRouterLifetime = cancel
		return lifetime
	}

	// The registry the content path reads, resolved per read rather than
	// captured here. Nothing has registered an engine at this point in boot and
	// a plugin may register one later, so a source captured now would be the
	// built-in one for the life of the process.
	contentSchemaSource := core.LazySchemaSource(hostBase.(core.SchemaSourceProvider).SchemaSource)

	// Router option builder
	// Closure so both the initial construction and the hot-reload callback
	// reuse the same option list.
	setupToken := firstRunSetupToken(ctx, cfg, db.NewUserStore(pool), logger)

	adminBaseOpts := func(routes []plugin.PluginRoutes) []api.RouterOption {
		opts := []api.RouterOption{
			api.WithPermissionChecker(hostBase.(core.PermissionCheckerProvider)),
			api.WithSchemaSource(contentSchemaSource),
			// Both tenant guards apply here too, because the admin router is
			// where content, media, users and schemas are authored. Without
			// them an archived tenant would stay writable, and a super_admin
			// X-Tenant-ID override naming a disabled tenant would be honored.
			api.WithArchivedChecker(archivedChecker),
			api.WithTenantValidator(tenantValidator),
			api.WithTenantRoster(tenantRoster),
			api.WithDefaultTenant(defaultTenant),
			api.WithMembershipStore(memberships),
			api.WithAdminTokenStore(adminTokens),
			// The request path sees TRUSTED_PROXIES only through this option.
			// See the API router.
			api.WithTrustedProxies(trustedCIDRs),
			api.WithPublicRateLimitOverrides(publicLimitOverrides, publicGlobalOverride),
			api.WithHealthProbes(healthReg),
			api.WithPluginStatus(activator),
			api.WithSecurityControls(securityControls{cfg: cfg, activator: activator, order: opts.ControlOrder}),
			api.WithPluginSchema(activator),
			api.WithPluginConfigReloader(activator),
			api.WithEntitlements(mgr),
			api.WithLicenseModule(licenseModule),
			api.WithPluginRoutes(routes),
			api.WithEndpointDocumenter(activator),
			api.WithAPIKeyAuth(apiKeyLookupMW),
			// The admin port logs every key request, as the API port does.
			api.WithAPIKeyAudit(apimw.APIKeyAudit(activator.APIKeyAuditLogFn(), trustedCIDRs)),
			api.WithMFAStore(activator.MFAStore()),
			api.WithLockoutBackend(lockoutBackend),
			api.WithRefreshTokenStore(refreshTokenStore),
			api.WithDeviceRiskAssessor(activator.RiskAssessor()),
			api.WithPoolHealthProvider(hostBase.(observability.PoolHealthProvider)),
			api.WithRequestCapture(activator.CaptureSink()),
			api.WithCapturePolicy(activator.CapturePolicy()),
			api.WithDSARAudit(activator.DSARAuditWriter()),
			api.WithDSARTenants(dsarTenantLister(tenantSlugs), dsarTenantScope(hostBase)),
			api.WithLatencyTracker(latencyTracker),
			api.WithScalingHost(hostBase),
			api.WithKeyStore(encKeyStore),
			api.WithSetupToken(setupToken),
			api.WithPreAuthMiddleware(preAuthMW()...),
		}
		owned := licenseRoutes(routes)
		for _, set := range owned {
			opts = append(opts, api.WithLicenseRoutes(set.Name, set.Routes))
		}
		// The admin router is built for every set of routes the engine
		// mounts, so the rate limit report is taken here, from the plugins'
		// routes and the licensing implementation's.
		publishEngineRateLimits(hostBase, cfg, append(routes[:len(routes):len(routes)], owned...))
		return opts
	}

	routerLifetime := newRouterLifetime()
	adminRouter, err := api.NewAdminRouter(pool, cfg,
		append(adminBaseOpts(pluginRoutes), append(adminExtraMW, api.WithLifetime(routerLifetime))...)...,
	)
	if err != nil {
		slog.Error("failed to build admin router", "error", err)
		return fmt.Errorf("build admin router: %w", err)
	}

	// API server (:3002)
	apiExtraMW := buildAPIExtraMW(ctx, cfg, activator, drainer, backpressureResult, latencyTracker)
	// Wire API key audit logging when a plugin provides it.
	if auditLogFn := activator.APIKeyAuditLogFn(); auditLogFn != nil {
		apiExtraMW = append(apiExtraMW, api.WithAPIKeyAudit(apimw.APIKeyAudit(auditLogFn, trustedCIDRs)))
	}

	// API router option builder
	apiBaseOpts := func(routes []plugin.PluginRoutes) []api.RouterOption {
		return []api.RouterOption{
			api.WithPermissionChecker(hostBase.(core.PermissionCheckerProvider)),
			api.WithSchemaSource(contentSchemaSource),
			// The content reads ask the host per request for the registered
			// core.ContentLocalizer, so a locale is honored while a plugin
			// provides one and ignored otherwise.
			api.WithContentLocalizer(hostBase.(core.ContentLocalizerProvider)),
			// Revision history belongs to the plugin that keeps the rows.
			// Read per request for the same reason as the localizer, and the
			// routes say so when no plugin keeps it.
			api.WithRecordRevisionStore(hostBase.(core.RecordRevisionStoreProvider)),
			api.WithContentStoreSink(func(cs *db.ContentStore) {
				// The API router owns the cached content store. Give the plugin
				// host that instance so a plugin's content write invalidates the
				// lists these reads are served from.
				// Wired before the host publishes it, so no plugin write
				// through the store races the hook being set.
				replicas.wireContent(cs)
				if setter, ok := hostBase.(interface {
					WithContentStore(*db.ContentStore)
				}); ok {
					setter.WithContentStore(cs)
				}
			}),
			// The routers need TRUSTED_PROXIES, not only the API-key audit
			// logger. Without it proxy headers are stripped whatever an
			// operator configured, and every per-IP limit counts the load
			// balancer rather than the caller behind it.
			api.WithTrustedProxies(trustedCIDRs),
			api.WithPublicRateLimitOverrides(publicLimitOverrides, publicGlobalOverride),
			api.WithHealthProbes(healthReg),
			api.WithEntitlements(mgr),
			api.WithPluginRoutes(routes),
			api.WithCustomRoutes(activator),
			api.WithAPIKeyAuth(apiKeyLookupMW),
			api.WithMFAStore(activator.MFAStore()),
			api.WithLockoutBackend(lockoutBackend),
			api.WithKeyStore(encKeyStore),
			api.WithArchivedChecker(archivedChecker),
			api.WithTenantValidator(tenantValidator),
			api.WithTenantRoster(tenantRoster),
			api.WithMembershipStore(memberships),
			api.WithRequestCapture(activator.CaptureSink()),
			api.WithCapturePolicy(activator.CapturePolicy()),
			api.WithPreAuthMiddleware(preAuthMW()...),
		}
	}

	apiRouter, err := api.NewAPIRouter(pool, cfg, hookRegistry,
		append(apiBaseOpts(pluginRoutes), append(apiExtraMW, api.WithLifetime(routerLifetime))...)...,
	)
	if err != nil {
		slog.Error("failed to build api router", "error", err)
		return fmt.Errorf("build api router: %w", err)
	}

	// Wrap both routers with SwappableHandler so hot-reload can swap the
	// underlying chi.Router atomically when plugin routes change.
	adminSwapper := api.NewSwappableHandler(adminRouter)
	apiSwapper := api.NewSwappableHandler(apiRouter)
	// A plugin that lets a tenant choose a URL asks which paths the API
	// router owns. The answer follows the swapper across hot reloads.
	if reg, ok := hostBase.(core.APIRoutesRegistrar); ok {
		reg.RegisterAPIRoutes(api.NewRouteIndex(apiSwapper.Current))
	}

	// Readiness gate
	// Block all non-health-check requests with 503 until the startup probe
	// passes at least once. This closes the window between ListenAndServe
	// (kernel accepts connections) and readiness (plugins signal ready).
	const readinessGateTimeout = 5 * time.Second
	readinessGate := api.ReadinessGateWithTimeout(healthReg, readinessGateTimeout)
	adminHandler := readinessGate(adminSwapper)
	apiHandler := readinessGate(apiSwapper)

	// Route-change callback: rebuilds both routers with updated plugin routes.
	// Uses the shared router-option closures to avoid duplicating option lists.
	activator.SetRoutesChangeCallback(func(routes []plugin.PluginRoutes) {
		logger.Info("hot-reload: rebuilding routers",
			"plugin_routes", len(routes))
		lifetime := newRouterLifetime()
		newAdmin, err := api.NewAdminRouter(pool, cfg,
			append(adminBaseOpts(routes), append(adminExtraMW, api.WithLifetime(lifetime))...)...,
		)
		if err != nil {
			logger.Error("hot-reload: failed to build admin router", "error", err)
			return
		}
		adminSwapper.Swap(newAdmin)

		newAPI, err := api.NewAPIRouter(pool, cfg, hookRegistry,
			append(apiBaseOpts(routes), append(apiExtraMW, api.WithLifetime(lifetime))...)...,
		)
		if err != nil {
			logger.Error("hot-reload: failed to build api router", "error", err)
			return
		}
		apiSwapper.Swap(newAPI)

		logger.Info("hot-reload: routers rebuilt")
	})

	// A license arriving after boot re-activates every gated plugin, and each
	// one rebuilds the handler its routes were bound to. The router mounted
	// those routes once, at boot, holding a method value that captured the
	// handler as it was then: for a plugin that booted unlicensed, its degraded
	// stub. Without re-collecting, the plugin activates, its store and its
	// registrations come up, and every one of its endpoints goes on answering
	// from the stub until the process restarts.
	//
	// Re-reading Routes() is what produces method values bound to the new
	// handlers. The callback above then rebuilds both routers from them and
	// swaps them in, which also covers a plugin that declares no routes at all
	// while degraded and so had nothing mounted to re-point.
	//
	// Registered here, after every plugin has started, so it runs after their
	// own license.changed handlers: RunSystem dispatches in registration order,
	// and this reads what those handlers have just rebuilt.
	hookRegistry.RegisterSystem("license.changed", func(ctx context.Context, _ map[string]any) error {
		routes := activator.RecollectRoutes()
		logger.Info("license changed: plugin routes re-collected", "plugins", len(routes))
		// The flow registry follows the same ordering: rebuilt here, after
		// every plugin has applied the new entitlement, so a provider that
		// withdrew its types on losing its license is out of the snapshot the
		// subscribers are told about.
		nodes, triggers := activator.RebuildFlowRegistry()
		logger.Info("license changed: flow registry rebuilt", "nodes", nodes, "triggers", triggers)
		return nil
	})

	// Security control liveness
	// The same evaluation the admin reads at /api/admin/security/controls,
	// run once here so a control that is configured but not enforcing is in
	// the log before the first request, and refused in strict mode.
	for _, iss := range issuersWithoutPolicy(cfg) {
		logger.Warn("trusted issuer has no policy in TRUSTED_ISSUER_POLICIES, so its tokens are ignored", "issuer", iss)
	}
	// The roster comes from the plugin that validates tenants. Without one
	// there is no roster to be off, so the check does not run and every pinned
	// issuer is left alone rather than warned about.
	var offRoster []string
	if tenantValidator != nil {
		var err error
		offRoster, err = issuersPinnedOffRoster(cfg, func(slug string) (bool, error) {
			return tenantValidator(ctx, slug), nil
		})
		if err != nil {
			logger.Warn("could not read the tenant roster to check TRUSTED_ISSUER_POLICIES", "err", err)
		}
	}
	for _, iss := range offRoster {
		logger.Warn("trusted issuer is pinned to a tenant this install does not serve, so its users will read nothing", "issuer", iss)
	}
	controls := securityControls{cfg: cfg, activator: activator, order: opts.ControlOrder}
	ctrlReport := controls.report()
	hasFailures := compliance.LogReport(logger, ctrlReport)
	if hasFailures {
		logger.Warn("one or more security controls are not enforced - see above for details. " +
			"Set CONTROL_STRICT_MODE=true to refuse boot on control failures.")
		if os.Getenv("CONTROL_STRICT_MODE") == "true" {
			return fmt.Errorf("control liveness check failed in strict mode:\n%s", compliance.FormatReport(ctrlReport))
		}
	}

	listenerTLS, err := listenerTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return err
	}

	adminSrv := &http.Server{
		Addr:         cfg.AdminListenAddr,
		Handler:      adminHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
		// ReadTimeout already bounds the whole read, so a slow header sender
		// cannot hold a connection open indefinitely. Stating the header
		// deadline separately makes that a property of the server rather than
		// a consequence of another setting, and caps the header block so a
		// sender cannot spend the read budget on headers alone.
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig:         listenerTLS,
	}
	go func() {
		logger.Info("admin server listening", "addr", cfg.AdminListenAddr, "tls", listenerTLS != nil)
		if err := serve(adminSrv); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin server error", "err", err)
		}
	}()

	apiSrv := &http.Server{
		Addr:         cfg.APIListenAddr,
		Handler:      apiHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
		// ReadTimeout already bounds the whole read, so a slow header sender
		// cannot hold a connection open indefinitely. Stating the header
		// deadline separately makes that a property of the server rather than
		// a consequence of another setting, and caps the header block so a
		// sender cannot spend the read budget on headers alone.
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig:         listenerTLS,
	}
	go func() {
		logger.Info("api server listening", "addr", cfg.APIListenAddr, "instance_id", cfg.InstanceID, "tls", listenerTLS != nil)
		if err := serve(apiSrv); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("api server error", "err", err)
		}
	}()

	// Open the readiness gate as soon as the probes pass. The gate's own
	// timeout is the backstop for a probe that never does, and past it
	// the probe is left to /startup and /readyz.
	go func() {
		listening := time.Now()
		awaitCtx, cancel := context.WithTimeout(ctx, readinessGateTimeout)
		defer cancel()
		if healthReg.AwaitStartup(awaitCtx, 25*time.Millisecond) {
			logger.Info("serving",
				"boot_ms", time.Since(runStarted).Milliseconds(),
				"after_listen_ms", time.Since(listening).Milliseconds())
		}
	}()

	// Graceful shutdown
	//
	// Shutdown sequence (budget-tracked against cfg.GracefulShutdownTimeout):
	//   0. Flip readiness to 503 + sleep PreStopDrainDelay (LB deregistration)
	//   1. Stop servers accepting new connections in parallel
	//   2. Drain in-flight requests with remaining budget
	//   3. Stop plugins with remaining budget after drain
	//   4. Deferred cleanup (telemetry flush, bus close, DB pool close)
	//
	// Each phase consumes from the global budget so the total shutdown
	// never exceeds GracefulShutdownTimeout regardless of how long
	// individual phases take.
	<-ctx.Done()
	logger.Info("shutting down", "timeout", cfg.GracefulShutdownTimeout)

	// 0. Flip readiness to 503 so load balancers deregister the pod.
	//    Sleep for the configured pre-stop delay to give the LB time
	//    to observe the readiness change before we stop accepting.
	healthReg.SetDraining()
	if cfg.PreStopDrainDelay > 0 {
		logger.Info("pre-stop drain delay", "delay", cfg.PreStopDrainDelay)
		time.Sleep(cfg.PreStopDrainDelay)
	}

	shutStart := time.Now()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), cfg.GracefulShutdownTimeout)
	defer shutCancel()

	// 1. Stop accepting new connections in parallel. Handlers already
	//    in-flight continue to run - Shutdown does not kill them.
	var eg errgroup.Group
	eg.Go(func() error {
		if err := adminSrv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("admin server shutdown: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		if err := apiSrv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("api server shutdown: %w", err)
		}
		return nil
	})
	if err := eg.Wait(); err != nil {
		logger.Error("server shutdown error", "err", err)
	}

	// 2. Drain in-flight requests with the REMAINING budget. If server
	//    shutdown consumed 30s of a 60s budget, drain gets at most 30s.
	remaining := time.Until(shutStart.Add(cfg.GracefulShutdownTimeout))
	if remaining <= 0 {
		logger.Warn("no time remaining for in-flight request drain")
	} else {
		drainStart := time.Now()
		if err := drainer.WaitOr(remaining); err != nil {
			logger.Warn("in-flight request drain timed out",
				"active_requests", drainer.ActiveCount(),
				"elapsed", time.Since(drainStart),
				"err", err)
		} else {
			logger.Info("in-flight requests drained",
				"elapsed", time.Since(drainStart))
		}
	}

	// 3. Stop plugins with a FRESH context carrying the remaining budget.
	//    Using the original shutCtx here would be a dead context if servers
	//    and drain consumed the entire timeout - plugins would never get
	//    a chance to gracefully release resources.
	pluginBudget := time.Until(shutStart.Add(cfg.GracefulShutdownTimeout))
	if pluginBudget <= 0 {
		logger.Warn("no time remaining for plugin shutdown - stopping with zero budget")
		pluginBudget = 1 * time.Second // last-resort grace for cleanup
	}
	pluginCtx, pluginCancel := context.WithTimeout(context.Background(), pluginBudget)
	defer pluginCancel()

	activatorNeedsStop = false // graceful path owns the stop from here
	var stopErr error
	if cfg.GoroutineEngineEnabled && parallel.MaxConcurrent() > 1 {
		stopErr = activator.StopParallel(pluginCtx, parallel)
	} else {
		stopErr = activator.Stop(pluginCtx)
	}
	if stopErr != nil {
		logger.Error("plugin activator stop returned error", "err", stopErr)
	}

	// Stop the backpressure pool-stats poller (idempotent, no-op when disabled).
	if backpressureResult.Stop != nil {
		backpressureResult.Stop()
	}

	// Scaling engine shutdown
	// Drain the worker pool, then cancel tracked goroutines and wait.
	workerPool.Shutdown()
	if err := tracker.Shutdown(5 * time.Second); err != nil {
		logger.Warn("goroutine tracker shutdown timed out", "err", err)
	}

	logger.Info("shutdown complete", "total_elapsed", time.Since(shutStart))
	return nil
}

// attachSessionPools gives the host its two side pools. RawDB serves a small
// DML-guarded pool, which stops plugins bypassing tenant isolation with raw
// DML through the *sql.DB handle. Migrations still use MigrationDB, the real
// pool, and admin DML goes through AdminQuerier. Advisory locks hold their
// sessions on a pool of their own: every plugin singleton keeps its lock
// while it leads, so if those sessions came from the guarded pool the first
// two leaders would take both connections, and the next plugin to ask for a
// lock would block its Start, and the boot, forever.
func attachSessionPools(ctx context.Context, host core.Host, engine, dsn string, logger *slog.Logger) {
	if dsn == "" {
		return
	}
	if setter, ok := host.(interface{ WithDMLGuardedDB(*sql.DB) }); ok {
		guard, err := db.NewDMLGuardDB(ctx, engine, dsn)
		if err != nil {
			logger.Warn("failed to create DML-guarded DB, so RawDB returns the raw pool",
				"err", err)
		} else {
			setter.WithDMLGuardedDB(guard)
		}
	}
	if setter, ok := host.(interface{ WithLockDB(*sql.DB) }); ok {
		locks, err := db.NewLockDB(ctx, engine, dsn)
		if err != nil {
			logger.Warn("failed to create the lock session pool - locks will hold sessions on the pool their caller passes",
				"err", err)
		} else {
			setter.WithLockDB(locks)
		}
	}
}
