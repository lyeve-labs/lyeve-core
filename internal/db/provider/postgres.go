package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
)

// PostgresProvider implements Provider for PostgreSQL.
type PostgresProvider struct{}

// Name returns the canonical engine name "postgres" for PostgreSQL.
func (p PostgresProvider) Name() string { return "postgres" }

// Dialect returns the PostgreSQL dialect for type mapping and DDL generation.
func (p PostgresProvider) Dialect() dialect.Dialect { return dialect.Postgres{} }

// DriverName returns the database/sql driver name "pgx" for PostgreSQL.
func (p PostgresProvider) DriverName() string { return "pgx" }

// DSNParser returns a URL-based DSN parser for PostgreSQL connection strings.
func (p PostgresProvider) DSNParser() DSNParser { return urlDSNParser{engine: "postgres"} }

// Capabilities returns the full set of feature flags that PostgreSQL supports.
func (p PostgresProvider) Capabilities() Capabilities {
	return NewCapabilities(
		CapCreateTableIfNotExists,
		CapAddColumnIfNotExists,
		CapDropColumnIfExists,
		CapCTE,
		CapCTERecursive,
		CapWindowFunc,
		CapJSONIndex,
		CapFullText,
		CapUpsertOnConflict,
		CapAdvisoryLock,
		CapRowLock,
		CapUUIDNative,
		CapJSONNative,
		CapTimestampTZ,
		CapArrayType,
		CapSavepoint,
		CapTwoPhaseCommit,
		CapListenNotify,
		CapPubSub,
		CapMaterializedView,
		CapIndexInclude,
		CapPartialIndex,
		CapParallelQuery,
		CapPartitionTable,
	)
}

// PoolDefaults returns recommended pool sizing parameters for PostgreSQL.
func (p PostgresProvider) PoolDefaults() PoolDefaults {
	return PoolDefaults{
		RecommendedMaxConns:        25,
		RecommendedMinConns:        2,
		RecommendedConnMaxLifetime: 1 * time.Hour,
		RecommendedConnMaxIdleTime: 5 * time.Minute,
		MaxConnLimit:               500,
	}
}

// Connect opens a new PostgreSQL connection pool for the given DSN.
func (p PostgresProvider) Connect(dsn string, opts PoolOptions) (db.DB, error) {
	opts = ApplyPoolOptions(opts, p.PoolDefaults())
	return db.ConnectWithOptions(context.Background(), dsn, db.ConnectOptions{
		MaxConns:          opts.MaxConns,
		MinConns:          opts.MinConns,
		ConnMaxLifetime:   opts.ConnMaxLifetime,
		ConnMaxIdleTime:   opts.ConnMaxIdleTime,
		HealthCheckPeriod: opts.HealthCheckPeriod,
	})
}

// ConnectWithFailover opens a PostgreSQL connection pool with replica failover.
func (p PostgresProvider) ConnectWithFailover(primaryDSN string, replicaDSNs []string, opts PoolOptions) (db.DB, error) {
	primary, err := p.Connect(primaryDSN, opts)
	if err != nil {
		return nil, fmt.Errorf("postgres failover primary: %w", err)
	}

	var replicas []db.DB
	for i, dsn := range replicaDSNs {
		r, err := p.Connect(dsn, opts)
		if err != nil {
			// Close already-opened replicas on partial failure
			for _, prev := range replicas {
				_ = prev.Close() // err suppressed: best-effort cleanup, replacing with new conn
			}
			_ = primary.Close() // err suppressed: best-effort cleanup
			return nil, fmt.Errorf("postgres failover replica[%d]: %w", i, err)
		}
		replicas = append(replicas, r)
	}

	return NewFailoverDB(FailoverConfig{
		Primary:       primary,
		Replicas:      replicas,
		CheckInterval: opts.HealthCheckPeriod,
	}), nil
}
