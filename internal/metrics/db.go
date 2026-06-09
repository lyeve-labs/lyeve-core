package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// DBCollector exposes database connection pool statistics as Prometheus gauges
// and counters. Implements prometheus.Collector so it can be registered on the
// default registry.
type DBCollector struct {
	stats func() DBStats

	maxOpen           *prometheus.Desc
	open              *prometheus.Desc
	inUse             *prometheus.Desc
	idle              *prometheus.Desc
	waitCount         *prometheus.Desc
	waitDuration      *prometheus.Desc
	maxIdleClosed     *prometheus.Desc
	maxIdleTimeCl     *prometheus.Desc
	maxLifetimeCl     *prometheus.Desc
	utilizationRatio  *prometheus.Desc
	exhaustionWarning *prometheus.Desc
}

// DBStats is a subset of sql.DBStats that the collector needs.
type DBStats struct {
	MaxOpenConnections int
	OpenConnections    int
	InUse              int
	Idle               int
	WaitCount          int64
	WaitDuration       float64 // seconds from time.Duration.Seconds()
	MaxIdleClosed      int64
	MaxIdleTimeClosed  int64
	MaxLifetimeClosed  int64
}

// ExhaustionThreshold is the utilization ratio that triggers the
// lyeve_db_pool_exhaustion_warning gauge. When InUse/MaxOpen exceeds this
// ratio, the gauge flips to 1. Configurable at package level so operators
// can tune it via env.
var ExhaustionThreshold = 0.8

// NewDBCollector creates a collector that reads from the provided stats function
// on every scrape. The stats function should be thread-safe.
func NewDBCollector(stats func() DBStats) *DBCollector {
	labels := []string{} // no labels: single pool per process
	return &DBCollector{
		stats: stats,
		maxOpen: prometheus.NewDesc(
			"lyeve_db_pool_max_connections",
			"Maximum number of open connections to the database.",
			labels, nil,
		),
		open: prometheus.NewDesc(
			"lyeve_db_pool_open_connections",
			"Number of established connections (in use + idle).",
			labels, nil,
		),
		inUse: prometheus.NewDesc(
			"lyeve_db_pool_in_use_connections",
			"Number of connections currently in use.",
			labels, nil,
		),
		idle: prometheus.NewDesc(
			"lyeve_db_pool_idle_connections",
			"Number of idle connections.",
			labels, nil,
		),
		waitCount: prometheus.NewDesc(
			"lyeve_db_pool_wait_count_total",
			"Total number of connections waited for.",
			labels, nil,
		),
		waitDuration: prometheus.NewDesc(
			"lyeve_db_pool_wait_duration_seconds_total",
			"Total time blocked waiting for a new connection, in seconds.",
			labels, nil,
		),
		maxIdleClosed: prometheus.NewDesc(
			"lyeve_db_pool_max_idle_closed_total",
			"Total number of connections closed due to SetMaxIdleConns.",
			labels, nil,
		),
		maxIdleTimeCl: prometheus.NewDesc(
			"lyeve_db_pool_max_idle_time_closed_total",
			"Total number of connections closed due to SetConnMaxIdleTime.",
			labels, nil,
		),
		maxLifetimeCl: prometheus.NewDesc(
			"lyeve_db_pool_max_lifetime_closed_total",
			"Total number of connections closed due to SetConnMaxLifetime.",
			labels, nil,
		),
		utilizationRatio: prometheus.NewDesc(
			"lyeve_db_pool_utilization_ratio",
			"Current pool utilization as a ratio (InUse / MaxOpenConnections).",
			labels, nil,
		),
		exhaustionWarning: prometheus.NewDesc(
			"lyeve_db_pool_exhaustion_warning",
			"1 when pool utilization exceeds the exhaustion threshold (default 80%), 0 otherwise.",
			labels, nil,
		),
	}
}

// Describe sends all metric descriptors to ch.
func (c *DBCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.maxOpen
	ch <- c.open
	ch <- c.inUse
	ch <- c.idle
	ch <- c.waitCount
	ch <- c.waitDuration
	ch <- c.maxIdleClosed
	ch <- c.maxIdleTimeCl
	ch <- c.maxLifetimeCl
	ch <- c.utilizationRatio
	ch <- c.exhaustionWarning
}

// Collect reads current pool stats and sends metric values to ch.
func (c *DBCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.stats()
	ch <- prometheus.MustNewConstMetric(c.maxOpen, prometheus.GaugeValue, float64(s.MaxOpenConnections))
	ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(s.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.inUse, prometheus.GaugeValue, float64(s.InUse))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(s.Idle))
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(s.WaitCount))
	ch <- prometheus.MustNewConstMetric(c.waitDuration, prometheus.CounterValue, s.WaitDuration)
	ch <- prometheus.MustNewConstMetric(c.maxIdleClosed, prometheus.CounterValue, float64(s.MaxIdleClosed))
	ch <- prometheus.MustNewConstMetric(c.maxIdleTimeCl, prometheus.CounterValue, float64(s.MaxIdleTimeClosed))
	ch <- prometheus.MustNewConstMetric(c.maxLifetimeCl, prometheus.CounterValue, float64(s.MaxLifetimeClosed))

	var util float64
	if s.MaxOpenConnections > 0 {
		util = float64(s.InUse) / float64(s.MaxOpenConnections)
	}
	ch <- prometheus.MustNewConstMetric(c.utilizationRatio, prometheus.GaugeValue, util)
	warning := 0.0
	if util > ExhaustionThreshold {
		warning = 1.0
	}
	ch <- prometheus.MustNewConstMetric(c.exhaustionWarning, prometheus.GaugeValue, warning)
}
