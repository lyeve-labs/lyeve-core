// Package enginehost provides the concrete implementation of the core.Host
// interface. It bridges in-process plugins to engine internals (the querier,
// the registered schema engine, the hook bus, configuration and per-plugin
// database provisioning) while enforcing the capability boundary that stops a
// plugin from reaching beyond the access it was granted.
package enginehost

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/cluster"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// NewHost constructs a concrete core.Host from the engine's internal types.
// The engine builds one host and shares it with every in-process plugin.
// The With* setters wire optional subsystems (storage, caches) after
// construction.
//
// The content store is built here over a reader that resolves per read,
// because an install has a registry only once a plugin registers an engine,
// which happens several boot steps after this.
func NewHost(pool db.DB, rawDB *sql.DB, cfg *config.Config, registry *hooks.Registry, version string) core.Host {
	h := &engineHost{
		pool:     pool,
		rawDB:    rawDB,
		cfg:      cfg,
		registry: registry,
		version:  version,
	}
	h.contentStore = db.NewContentStore(pool, core.LazySchemaSource(h.SchemaSource))
	return h
}

type engineHost struct {
	pool      db.DB
	rawDB     *sql.DB
	guardedDB *sql.DB // DML-guarded pool for RawDB, or nil to hand out rawDB
	lockDB    *sql.DB // session pool for DistLock, or nil for the caller's pool
	cfg       *config.Config
	registry  *hooks.Registry
	version   string

	// Wired by WithStorage. Nil-safe: plugins that need storage
	// type-assert the host to StorageProvider.
	storage core.Storage

	// suppliedSchema is the only engine there is. Nothing here builds one.

	suppliedSchema core.SchemaEngine

	// Built at construction over a reader that resolves per read, so a plugin
	// registering an engine later feeds it.
	contentStore *db.ContentStore

	// Wired by WithPoolHealth. Nil-safe: pool health endpoint returns
	// "not configured" when unwired.
	poolHealth *poolHealthCfg

	// Wired by SetUserProvisioner. Nil-safe: UserProvisioner() returns nil
	// until a plugin registers one.
	userProvisioner core.UserProvisioner

	// Wired by SetAdminSeatMembers (runtime boot, once the membership
	// reader is resolved). Atomic because plugins that started earlier may
	// already be serving a write. Until then the admin seat guard counts the
	// accounts' own roles only.
	seatMembers atomic.Pointer[membershipBox]

	// Wired by RegisterEmailSender (the registering plugin's Start). Nil-safe:
	// EmailSender() returns nil when no plugin registered one.
	emailSender core.EmailSender

	// Wired by RegisterFlowDefinitionValidator (the registering plugin's
	// Start, cleared in its Stop). Nil-safe: FlowDefinitionValidator()
	// returns nil when no plugin has registered one.
	flowValidator core.FlowDefinitionValidator

	// Wired by RegisterFlowInvoker (the registering plugin's activation,
	// cleared when it deactivates). Nil-safe: FlowInvoker() returns nil when
	// no plugin has registered one, and a transport then exposes no flows.
	flowInvoker core.FlowInvoker

	// Wired by RegisterAPIRoutes (runtime boot, once the API router exists).
	// Atomic because the runtime registers it while plugins that started
	// earlier may already be validating a path. Nil-safe: APIRoutes()
	// returns nil until then.
	apiRoutes atomic.Pointer[apiRoutesBox]

	// Wired by RegisterPermissionChecker (the plugin that owns the rules, in
	// its Start, cleared in its Stop). PermissionChecker() never returns nil:
	// while this is nil it answers core.RoleOnlyPermissionChecker.
	permissionChecker core.PermissionChecker

	// Wired by RegisterContentLocalizer (the registering plugin's Start,
	// cleared in its Stop). Nil-safe: ContentLocalizer() returns nil when no
	// plugin has registered one, and a content read then ignores any locale.
	contentLocalizer core.ContentLocalizer

	// configSections holds the configuration sections plugins register in
	// Start and remove in Stop. The zero value is an empty registry.
	configSections core.ConfigSectionRegistry

	// Wired by RegisterContentEntryWriter (the registering plugin's
	// activation, cleared in its Stop). Nil-safe: ContentEntryWriter()
	// returns nil when no plugin has registered one, and an import then
	// writes the generated table directly.
	contentEntryWriter core.ContentEntryWriter

	// recordRevisionStore is the plugin that keeps the schema-driven content
	// API's revision snapshots. Nil until one registers, and nil again when
	// it stops.
	recordRevisionStore core.RecordRevisionStore

	// Wired by RegisterMetricsGatherer (runtime boot, after the registry
	// and the pool collector exist). Nil-safe: MetricsGatherer() returns nil
	// until then, and an exporter then has nothing to ship.
	metricsGatherer core.MetricsGatherer

	// Wired by SetRefreshTokenRevoker (runtime boot). Nil-safe:
	// RevokeAllRefreshTokens is a no-op when refresh tokens are not configured.
	revokeRefreshFn func(ctx context.Context, userID string) error

	// Wired by WithQueryCache. Nil-safe: caching is transparently applied
	// to QuerierRO reads. When nil, reads go through to the pool directly.
	queryCache *db.QueryCache

	// Wired by WithClusterBus. queryInvalidations carries InvalidateCache
	// prefixes to the other replicas, whose query caches would otherwise
	// serve the old result until their TTL.
	clusterBus         cluster.Bus
	queryInvalidations *cluster.Coalescer

	// Wired by WithStorageConnected. Nil-safe: StorageConnected() returns nil
	// when no storage provider (S3 or MinIO) is connected. Distinct from
	// core.Storage, which is the local storage driver used by assets/content.
	storageConnected any

	// Lazy-init tenancy strategy for AcquireTenantConn (gRPC tenant isolation).
	// Initialized on first call from pool.Engine(): single-tenant deployments
	// with no MultiTenant config never allocate one.
	tenancyOnce sync.Once
	tenancyVal  db.Tenancy

	// Scaling components. Nil means not wired.
	workerPool *engine.WorkerPool
	tracker    *engine.GoroutineTracker
	parallel   *engine.ParallelEngine
	asyncHooks *engine.AsyncHookExecutor

	// Wired by WithLogRing (runtime boot). Nil until then, and a plugin that
	// reads it answers that the ring is unavailable.
	logRing core.LogRing

	// Wired by WithEngineRateLimits each time the runtime builds the routers,
	// because the limits the mounted routes declare follow the plugins that
	// run. Atomic because a rebuild publishes a new list while a request may
	// be reading the one before it. Nil until the first build.
	engineRateLimits atomic.Pointer[[]core.EngineRateLimit]

	// Wired by WithLicensing (runtime boot). Nil-safe: Capabilities()
	// answers the set of an install with no license when nil.
	licensing licensing.Manager

	// Wired by WithFlowRegistry (runtime boot, before plugins start). Nil
	// until then: a host built without an activator has no started plugins
	// to collect from, and FlowRegistry() says so by returning nil.
	flowRegistry core.FlowRegistry

	// Wired by WithTenantRegions (runtime boot, before plugins start). The
	// host asks it on every TenantRegionResolver call rather than keeping the
	// answer, because the plugin that holds the role may start after the
	// plugin asking, or stop while it runs. Nil: no region is known.
	tenantRegions core.TenantRegionResolverProvider
}

// Compile-time assertions that engineHost satisfies all required interfaces
// the plugin system may type-assert against.
var (
	_ core.TenancyConnProvider  = (*engineHost)(nil)
	_ core.EmailSenderProvider  = (*engineHost)(nil)
	_ core.EmailSenderRegistrar = (*engineHost)(nil)
	_ core.RefreshTokenRevoker  = (*engineHost)(nil)
	_ core.FlowRegistryHost     = (*engineHost)(nil)

	_ core.FlowDefinitionValidatorProvider  = (*engineHost)(nil)
	_ core.FlowDefinitionValidatorRegistrar = (*engineHost)(nil)
	_ core.FlowInvokerProvider              = (*engineHost)(nil)
	_ core.FlowInvokerRegistrar             = (*engineHost)(nil)
	_ core.PermissionCheckerProvider        = (*engineHost)(nil)
	_ core.PermissionCheckerRegistrar       = (*engineHost)(nil)
	_ core.ContentLocalizerProvider         = (*engineHost)(nil)
	_ core.ContentLocalizerRegistrar        = (*engineHost)(nil)
	_ core.ConfigSectionProvider            = (*engineHost)(nil)
	_ core.ConfigSectionRegistrar           = (*engineHost)(nil)
	_ core.ContentEntryWriterProvider       = (*engineHost)(nil)
	_ core.ContentEntryWriterRegistrar      = (*engineHost)(nil)
	_ core.MetricsGathererProvider          = (*engineHost)(nil)
	_ core.MetricsGathererRegistrar         = (*engineHost)(nil)
	_ core.GoroutineTunablesProvider        = (*engineHost)(nil)
	_ core.LogRingProvider                  = (*engineHost)(nil)
	_ core.EngineRateLimitsProvider         = (*engineHost)(nil)
	_ core.APIRoutesProvider                = (*engineHost)(nil)
	_ core.APIRoutesRegistrar               = (*engineHost)(nil)
	_ core.TenantRegionResolverProvider     = (*engineHost)(nil)
)

// core.Host interface methods

func (h *engineHost) Querier(ctx context.Context) core.Querier {
	q := &poolQuerier{pool: h.pool}
	if core.DebugQuerierFunc != nil {
		return core.DebugQuerierFunc(ctx, q, h.pool.Engine(), h.rawDB)
	}
	return q
}

func (h *engineHost) QuerierRO(ctx context.Context) core.Querier {
	ro, err := h.pool.QuerierRO(ctx)
	if err != nil {
		// Log and fall back to primary: read replicas are best-effort.
		slog.Error("querier_ro: falling back to primary", "err", err)
		return &poolQuerier{pool: h.pool}
	}
	return &roQuerier{ro: ro}
}

func (h *engineHost) Logger(_ context.Context) *slog.Logger {
	return slog.Default()
}

func (h *engineHost) Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// CachedFetch implements QueryCacheProvider.CachedFetch. Concurrent callers
// that miss the same key run the fetcher once and share the result. See
// db.QueryCache.FetchInto for the coalescing rules.
func (h *engineHost) CachedFetch(ctx context.Context, pluginName, tenantID, dialect, sql string, args []any, ttl time.Duration, dest any, fn core.CacheFetcher) (bool, error) {
	if h.queryCache == nil {
		data, err := fn()
		if err != nil {
			return false, err
		}
		if dest != nil {
			if err := json.Unmarshal(data, dest); err != nil {
				return false, fmt.Errorf("cached_fetch unmarshal: %w", err)
			}
		}
		return false, nil
	}

	key := db.BuildCacheKey(pluginName, dialect, tenantID, sql, args)
	return h.queryCache.FetchInto(ctx, key, ttl, dest, fn)
}

// InvalidateCache implements QueryCacheProvider.InvalidateCache. The prefix
// also goes to the other replicas, which drop it through
// InvalidateCacheLocal.
func (h *engineHost) InvalidateCache(prefix string) int {
	h.queryInvalidations.Mark(prefix)
	return h.InvalidateCacheLocal(prefix)
}

// InvalidateCacheLocal drops prefix from this replica's query cache only. The
// bus subscriber calls it, so a received invalidation is not sent back out.
func (h *engineHost) InvalidateCacheLocal(prefix string) int {
	if h.queryCache == nil {
		return 0
	}
	return h.queryCache.Invalidate(prefix)
}

// WithClusterBus wires the bus the host hands plugins and uses for its own
// cross-replica invalidations.
func (h *engineHost) WithClusterBus(b cluster.Bus) {
	h.clusterBus = b
	if b != nil {
		h.queryInvalidations = cluster.NewCoalescer(b, TopicQueryCacheInvalidate, 250*time.Millisecond)
	}
}

// ClusterBus implements core.ClusterBusProvider.
func (h *engineHost) ClusterBus() cluster.Bus { return h.clusterBus }

// TopicQueryCacheInvalidate carries query cache prefixes between replicas.
const TopicQueryCacheInvalidate = "core.querycache.invalidate"

func (h *engineHost) Config() core.Config {
	return &configAdapter{cfg: h.cfg}
}

func (h *engineHost) Hooks() core.HookBus {
	bus := core.HookBus(&hookBusAdapter{registry: h.registry})
	if core.DebugHookBusFunc != nil {
		bus = core.DebugHookBusFunc(bus)
	}
	return bus
}

// HookPublisher returns the same hook bus adapter, typed as
// core.HookPublisher. Only trusted callers should call Publish through this
// interface.
func (h *engineHost) HookPublisher() core.HookPublisher {
	pub := core.HookPublisher(&hookBusAdapter{registry: h.registry})
	if core.DebugHookPublisherFunc != nil {
		pub = core.DebugHookPublisherFunc(pub)
	}
	return pub
}

func (h *engineHost) Version() string {
	return h.version
}

// Dialect returns the database dialect name.
func (h *engineHost) Dialect() string { return h.pool.Engine() }

// Scaling methods return wired components or nil.
func (h *engineHost) WorkerPool() *engine.WorkerPool               { return h.workerPool }
func (h *engineHost) GoroutineTracker() *engine.GoroutineTracker   { return h.tracker }
func (h *engineHost) ParallelEngine() *engine.ParallelEngine       { return h.parallel }
func (h *engineHost) AsyncHookExecutor() *engine.AsyncHookExecutor { return h.asyncHooks }

// DistLock is nil on a stateless engine: the lock is a database advisory lock
// and there is no database to hold it. A caller treats nil as running
// unlocked, so every replica does the work.
func (h *engineHost) DistLock(name string) *engine.DistLock {
	if h.cfg != nil && h.cfg.Stateless() {
		return nil
	}
	return engine.NewDistLockOn(name, h.lockDB)
}

// GoroutineTunables implements GoroutineTunablesProvider over the scaling
// primitives this host was built with.
func (h *engineHost) GoroutineTunables() core.GoroutineTunables { return core.NewGoroutineTunables(h) }

// WithEngineRateLimits hands the host the limits the engine enforces for the
// routes the routers mount. The runtime computes them from the same parse and
// the same declarations the routers use, so the list cannot disagree with
// what runs. A later call replaces the list.
func (h *engineHost) WithEngineRateLimits(limits []core.EngineRateLimit) {
	own := append([]core.EngineRateLimit(nil), limits...)
	h.engineRateLimits.Store(&own)
}

// EngineRateLimits implements EngineRateLimitsProvider. It returns a copy so
// no caller can edit the host's snapshot.
func (h *engineHost) EngineRateLimits() []core.EngineRateLimit {
	limits := h.engineRateLimits.Load()
	if limits == nil {
		return nil
	}
	return append([]core.EngineRateLimit(nil), (*limits)...)
}

// WithLogRing hands the host the ring the slog chain feeds. The runtime
// builds the ring before the host exists, since logging comes up first.
func (h *engineHost) WithLogRing(ring core.LogRing) { h.logRing = ring }

// LogRing implements LogRingProvider. Nil until the runtime wires one.
func (h *engineHost) LogRing() core.LogRing { return h.logRing }

// DeclaredResources reads one resource section of the configuration file the
// engine booted from. It is read on each call rather than cached, since only a
// plugin's Start calls it.
func (h *engineHost) DeclaredResources(section string) ([]json.RawMessage, error) {
	return config.LoadDeclaredSection("", section)
}
