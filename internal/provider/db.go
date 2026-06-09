package provider

import (
	"context"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	dbprovider "github.com/lyeve-labs/lyeve-core/internal/db/provider"
)

// DBProvider extends Provider with database-specific operations.
type DBProvider interface {
	Provider

	// Connect opens a new connection pool for the given DSN.
	Connect(ctx context.Context, dsn string, opts PoolOptions) (db.DB, error)

	// ConnectWithFailover opens a connection pool with replica failover.
	ConnectWithFailover(ctx context.Context, primaryDSN string, replicaDSNs []string, opts PoolOptions) (db.DB, error)

	// DriverName returns the database/sql driver name for sql.Open.
	DriverName() string
}

// PoolOptions carries provider-agnostic pool-tuning parameters.
type PoolOptions struct {
	MaxConns          int32
	MinConns          int32
	ConnMaxLifetime   time.Duration
	ConnMaxIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// DefaultPoolOptions returns sensible defaults for any engine.
func DefaultPoolOptions() PoolOptions {
	return PoolOptions{
		MaxConns:          25,
		MinConns:          2,
		ConnMaxLifetime:   1 * time.Hour,
		ConnMaxIdleTime:   5 * time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	}
}

// dbProviderAdapter bridges a db/provider.Provider into the unified
// Provider interface. It delegates Name(), Capabilities(), etc., and adds
// HealthCheck() via a lightweight ping-and-close cycle.
type dbProviderAdapter struct {
	inner dbprovider.Provider
}

// newDBProviderAdapter creates a unified wrapper around a db/provider.Provider.
func newDBProviderAdapter(inner dbprovider.Provider) *dbProviderAdapter {
	return &dbProviderAdapter{inner: inner}
}

func (a *dbProviderAdapter) Name() string       { return a.inner.Name() }
func (a *dbProviderAdapter) Category() Category { return CategoryDB }

func (a *dbProviderAdapter) Capabilities() Capabilities {
	legacy := a.inner.Capabilities()
	return NewCapabilities(uint64(legacy.Mask()))
}

// HealthCheck is a provider-level check that verifies the provider type is
// registered. It does not test a specific connection: FailoverDB monitors
// actual connection health on the active pool.
func (a *dbProviderAdapter) HealthCheck(ctx context.Context) error {
	return nil
}

// DetailedHealth returns richer diagnostic info for this provider type.
func (a *dbProviderAdapter) DetailedHealth(ctx context.Context) HealthStatus {
	return HealthStatus{
		Provider:  a.Name(),
		Category:  CategoryDB,
		Healthy:   true,
		CheckedAt: time.Now(),
	}
}

func (a *dbProviderAdapter) AutoDetect(dsn string) bool {
	engine := detectEngineFromDSN(dsn)
	return engine == a.inner.Name()
}

func (a *dbProviderAdapter) Connect(ctx context.Context, dsn string, opts PoolOptions) (db.DB, error) {
	legacyOpts := dbprovider.PoolOptions{
		MaxConns:          opts.MaxConns,
		MinConns:          opts.MinConns,
		ConnMaxLifetime:   opts.ConnMaxLifetime,
		ConnMaxIdleTime:   opts.ConnMaxIdleTime,
		HealthCheckPeriod: opts.HealthCheckPeriod,
	}
	return a.inner.Connect(dsn, legacyOpts)
}

func (a *dbProviderAdapter) ConnectWithFailover(ctx context.Context, primaryDSN string, replicaDSNs []string, opts PoolOptions) (db.DB, error) {
	legacyOpts := dbprovider.PoolOptions{
		MaxConns:          opts.MaxConns,
		MinConns:          opts.MinConns,
		ConnMaxLifetime:   opts.ConnMaxLifetime,
		ConnMaxIdleTime:   opts.ConnMaxIdleTime,
		HealthCheckPeriod: opts.HealthCheckPeriod,
	}
	return a.inner.ConnectWithFailover(primaryDSN, replicaDSNs, legacyOpts)
}

func (a *dbProviderAdapter) DriverName() string { return a.inner.DriverName() }

// ConnectedDB wraps an active db.DB with its provider metadata so health
// checks can ping the live pool and cost tracking can attribute operations.
type ConnectedDB struct {
	DB       db.DB
	Provider DBProvider
	Costs    CostTracker
}

// HealthCheck pings the active pool.
func (c *ConnectedDB) HealthCheck(ctx context.Context) error {
	return c.DB.Ping(ctx)
}

// DetailedHealth returns pool statistics alongside ping latency.
func (c *ConnectedDB) DetailedHealth(ctx context.Context) HealthStatus {
	start := time.Now()
	err := c.DB.Ping(ctx)
	elapsed := time.Since(start)

	stats := c.DB.Stats()
	status := HealthStatus{
		Provider:  c.Provider.Name(),
		Category:  CategoryDB,
		Healthy:   err == nil,
		Latency:   elapsed,
		CheckedAt: start,
	}
	if err != nil {
		status.Error = err.Error()
	}
	_ = stats // pool stats not yet wired into HealthStatus
	return status
}

// RecordOp tracks a database operation in the cost tracker.
func (c *ConnectedDB) RecordOp(operation string, latency time.Duration, err error) {
	cost := OperationCost{
		Provider:  c.Provider.Name(),
		Category:  CategoryDB,
		Operation: operation,
		Latency:   latency,
		Timestamp: time.Now(),
	}
	if err != nil {
		cost.Error = err.Error()
	}
	c.Costs.Record(cost)
}

// detectEngineFromDSN is a lightweight DSN parser that extracts the engine
// name without importing internal/db.
func detectEngineFromDSN(dsn string) string {
	if idx := strings.Index(dsn, "://"); idx >= 0 {
		scheme := strings.ToLower(dsn[:idx])
		switch scheme {
		case "postgres", "postgresql":
			if strings.Contains(dsn, "cluster=") || strings.Contains(dsn, "cockroach") {
				return "cockroachdb"
			}
			return "postgres"
		case "mysql", "tidb":
			return "mysql"
		case "mssql", "sqlserver":
			return "mssql"
		case "sqlite":
			return "sqlite"
		}
	}
	if strings.Contains(dsn, "@tcp(") {
		return "mysql"
	}
	if strings.HasPrefix(dsn, "file:") {
		return "sqlite"
	}
	return "postgres"
}

// RegisterDBProviders bridges every registered db/provider into the
// unified Registry. Call once during boot after db/provider init() runs.
func RegisterDBProviders() {
	for _, name := range dbprovider.Registry.Names() {
		p := dbprovider.Registry.Get(name)
		if p == nil {
			continue
		}
		Registry.Register(newDBProviderAdapter(p))
	}
}
