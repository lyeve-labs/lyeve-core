// Package metrics provides Prometheus instrumentation for LyEve CMS.
//
// Metrics are labeled by plugin name, HTTP method, status code, and tenant,
// giving per-plugin, per-tenant observability at the /metrics endpoint. The
// tenant label is the slug the request resolved to, so its cardinality is
// bounded by the tenant count. A request that resolved no tenant carries
// NoTenantLabel.
//
// Integration:
//   - Init() creates the default Registry for the process.
//   - middleware.Record() is called by the request-logging middleware after
//     each request, recording plugin and tenant labels from the request context.
//   - Handler() returns an http.Handler for the /metrics endpoint via promhttp.
//   - NewDBCollector wraps db.DB.Stats() as a Prometheus collector.
package metrics

import (
	"net/http"
	"sync"

	"github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Labels

// PluginCtxKey is the context key for the plugin name label.
// Injected by mountPluginRoutes before dispatching to a plugin handler.
const PluginCtxKey ctxKey = "metrics_plugin"

type ctxKey string

// PluginLabel returns the plugin label for the current request, or "core"
// when no plugin label is set (requests handled by the core engine itself).
//
// The context value is what a handler below the dispatch sees. The
// recording middleware wraps the whole chain and holds the request it was
// given, so it reads the slot the dispatch filled in instead, the same way
// TenantLabel reads the tenant slot.
func PluginLabel(r *http.Request) string {
	if v, ok := r.Context().Value(PluginCtxKey).(string); ok && v != "" {
		return v
	}
	return core.PluginSlotFrom(r.Context()).PluginOr(core.EnginePluginLabel)
}

// NoTenantLabel is the tenant label of a request that resolved no tenant:
// a public route, a probe, or a request refused before tenancy ran.
const NoTenantLabel = "-"

// TenantLabel returns the tenant label for the current request: the slug
// on the context, else the slug the tenant middleware reported through the
// request's TenantSlot, else NoTenantLabel.
//
// The slot matters because the recording middleware wraps the whole chain
// and holds the context it was given, while tenancy is resolved further
// down on a derived context it never sees. Read from the outer context
// alone, every request would be recorded as belonging to no tenant.
func TenantLabel(r *http.Request) string {
	ctx := r.Context()
	if tid := middleware.TenantIDFromContext(ctx); tid != "" {
		return tid
	}
	if tid := core.TenantSlotFrom(ctx).Get(); tid != "" {
		return tid
	}
	return NoTenantLabel
}

// Registry and metrics

var (
	// mu serializes reads and writes to the package-level globals below.
	// Prometheus collectors themselves are goroutine-safe. The race is
	// only on which pointer is assigned to the package var.  Callers
	// snapshot the pointer under mu, release, and then use the snapshot.
	mu sync.Mutex

	// initOnce guards metrics initialization so Init() is idempotent.
	initOnce sync.Once

	// reg is the global Prometheus registry for this process.
	reg *prometheus.Registry

	// RequestsTotal is a counter partitioned by plugin, method, status code,
	// and tenant.
	RequestsTotal *prometheus.CounterVec

	// RequestDurationSeconds is a histogram of request durations partitioned
	// by plugin, method, and tenant.
	RequestDurationSeconds *prometheus.HistogramVec

	// RequestsInFlight is a gauge tracking concurrent in-flight requests
	// per plugin and tenant.
	RequestsInFlight *prometheus.GaugeVec

	// ContentListCacheHits counts cache hits on ContentStore.List.
	ContentListCacheHits prometheus.Counter

	// ContentListCacheMisses counts cache misses on ContentStore.List.
	ContentListCacheMisses prometheus.Counter
)

// DefaultBucketsSec defines histogram buckets appropriate for HTTP API latency
// (in seconds): 1ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s.
var DefaultBucketsSec = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Init initializes the global Prometheus registry and registers the standard
// process + Go collectors plus LyEve-specific metric vectors.
// Safe for concurrent access: subsequent calls after the first are no-ops.
// Must be called once at startup, before any requests are served.
func Init() *prometheus.Registry {
	mu.Lock()
	defer mu.Unlock()
	initOnce.Do(func() {
		reg = prometheus.NewRegistry()

		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		reg.MustRegister(collectors.NewGoCollector())

		RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "lyeve",
			Name:      "requests_total",
			Help:      "Total number of HTTP requests processed, partitioned by plugin, method, status code, and tenant.",
		}, []string{"plugin", "method", "code", "tenant"})
		reg.MustRegister(RequestsTotal)

		RequestDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "lyeve",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency in seconds, partitioned by plugin, method, and tenant.",
			Buckets:   DefaultBucketsSec,
		}, []string{"plugin", "method", "tenant"})
		reg.MustRegister(RequestDurationSeconds)

		RequestsInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "lyeve",
			Name:      "requests_in_flight",
			Help:      "Number of concurrent HTTP requests currently being served, per plugin and tenant.",
		}, []string{"plugin", "tenant"})
		reg.MustRegister(RequestsInFlight)

		ContentListCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "lyeve",
			Name:      "content_list_cache_hits_total",
			Help:      "Total number of ContentStore.List cache hits.",
		})
		reg.MustRegister(ContentListCacheHits)

		ContentListCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "lyeve",
			Name:      "content_list_cache_misses_total",
			Help:      "Total number of ContentStore.List cache misses.",
		})
		reg.MustRegister(ContentListCacheMisses)
	})
	return reg
}

// Registry returns the global prometheus.Registry. Nil if Init() was not called.
func Registry() *prometheus.Registry {
	mu.Lock()
	defer mu.Unlock()
	return reg
}

// ResetForTesting resets the global metrics state so each test starts
// with a clean slate. Must only be called from test code: never in
// production. Resets the sync.Once gate so Init() can be called again.
func ResetForTesting() {
	mu.Lock()
	defer mu.Unlock()
	initOnce = sync.Once{}
	reg = nil
	RequestsTotal = nil
	RequestDurationSeconds = nil
	RequestsInFlight = nil
	ContentListCacheHits = nil
	ContentListCacheMisses = nil
}

// Handler returns an http.Handler that serves the /metrics endpoint in
// Prometheus text format.
func Handler() http.Handler {
	// Snapshot reg under lock, and lazy-init outside to avoid reentry deadlock.
	mu.Lock()
	r := reg
	mu.Unlock()
	if r == nil {
		r = Init() // Init() does its own locking
	}
	return promhttp.HandlerFor(r, promhttp.HandlerOpts{
		Registry: r,
	})
}

// Recording helpers

// RecordRequest updates the Prometheus metrics for a completed request.
// Called by the structured-logger middleware after ServeHTTP returns.
func RecordRequest(r *http.Request, status int, durationSec float64) {
	mu.Lock()
	total := RequestsTotal
	dur := RequestDurationSeconds
	inFlight := RequestsInFlight
	mu.Unlock()

	if total == nil || dur == nil || inFlight == nil {
		return // not initialized
	}
	plugin := PluginLabel(r)
	tenant := TenantLabel(r)
	method := r.Method
	code := statusCodeBucket(status)

	total.WithLabelValues(plugin, method, code, tenant).Inc()
	dur.WithLabelValues(plugin, method, tenant).Observe(durationSec)
}

// RecordInFlightStart increments the in-flight gauge for the plugin and tenant.
// Returns a function that must be called when the request completes
// to decrement the gauge.
func RecordInFlightStart(r *http.Request) func() {
	mu.Lock()
	inFlight := RequestsInFlight
	mu.Unlock()

	if inFlight == nil {
		return func() {}
	}
	plugin := PluginLabel(r)
	tenant := TenantLabel(r)
	inFlight.WithLabelValues(plugin, tenant).Inc()
	return func() {
		inFlight.WithLabelValues(plugin, tenant).Dec()
	}
}

func statusCodeBucket(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
