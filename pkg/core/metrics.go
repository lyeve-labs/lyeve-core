package core

import (
	dto "github.com/prometheus/client_model/go"
)

// MetricsGatherer is the engine's own metrics registry, read-only. It is
// what /api/admin/metrics serves: the process and Go collectors, the
// request counter, latency histogram and in-flight gauge, and the
// lyeve_db_pool_* series. A plugin that ships metrics to a remote system
// gathers this registry instead of keeping one of its own, so what it
// exports is what the scrape endpoint shows.
//
// The method set is the Prometheus Gatherer's, so a value satisfies
// prometheus.Gatherer and can be handed to promhttp or an exporter without
// an adapter. A plugin never registers a collector through it: the
// registry is the engine's, and a plugin's own series live in a registry
// it owns.
type MetricsGatherer interface {
	Gather() ([]*dto.MetricFamily, error)
}

// MetricsGathererRegistrar is implemented by the engine host. The runtime
// calls it once at boot, after the registry exists and the pool collector
// is registered on it, so a plugin that starts later sees every series.
type MetricsGathererRegistrar interface {
	RegisterMetricsGatherer(g MetricsGatherer)
}

// MetricsGathererProvider is implemented by the engine host and forwarded
// by ScopedHost to a plugin that holds CapMetrics. Nil means the runtime
// has not registered the registry, which only a host built outside the
// engine does. A plugin then exports nothing and says so.
type MetricsGathererProvider interface {
	MetricsGatherer() MetricsGatherer
}
