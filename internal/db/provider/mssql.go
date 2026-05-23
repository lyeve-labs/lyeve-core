package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
)

// MSSQLProvider implements Provider for SQL Server 2019+ / Azure SQL.
type MSSQLProvider struct{}

// Name returns the canonical engine name "mssql" for SQL Server.
func (p MSSQLProvider) Name() string { return "mssql" }

// Dialect returns the MSSQL dialect for type mapping and DDL generation.
func (p MSSQLProvider) Dialect() dialect.Dialect { return dialect.MSSQL{} }

// DriverName returns the database/sql driver name "sqlserver" for MSSQL.
func (p MSSQLProvider) DriverName() string { return "sqlserver" }

// DSNParser returns an MSSQL-specific DSN parser for connection strings.
func (p MSSQLProvider) DSNParser() DSNParser { return mssqlDSNParser{} }

// Capabilities returns the set of feature flags that MSSQL 2019+ supports.
func (p MSSQLProvider) Capabilities() Capabilities {
	return NewCapabilities(
		CapCTE,
		CapCTERecursive,
		CapWindowFunc,
		CapFullText,
		CapUpsertMerge,
		CapAdvisoryLock,
		CapRowLock,
		CapSavepoint,
		CapTwoPhaseCommit,
		CapMaterializedView,
		CapIndexInclude,
		CapPartitionTable,
		CapParallelQuery,
	)
}

// PoolDefaults returns recommended pool sizing parameters for MSSQL.
func (p MSSQLProvider) PoolDefaults() PoolDefaults {
	return PoolDefaults{
		RecommendedMaxConns:        30,
		RecommendedMinConns:        2,
		RecommendedConnMaxLifetime: 1 * time.Hour,
		RecommendedConnMaxIdleTime: 5 * time.Minute,
		MaxConnLimit:               200,
	}
}

// Connect opens a new MSSQL connection pool for the given DSN.
func (p MSSQLProvider) Connect(dsn string, opts PoolOptions) (db.DB, error) {
	opts = ApplyPoolOptions(opts, p.PoolDefaults())
	return db.ConnectWithOptions(context.Background(), dsn, db.ConnectOptions{
		MaxConns:          opts.MaxConns,
		MinConns:          opts.MinConns,
		ConnMaxLifetime:   opts.ConnMaxLifetime,
		ConnMaxIdleTime:   opts.ConnMaxIdleTime,
		HealthCheckPeriod: opts.HealthCheckPeriod,
	})
}

// ConnectWithFailover opens an MSSQL connection pool with replica failover.
func (p MSSQLProvider) ConnectWithFailover(primaryDSN string, replicaDSNs []string, opts PoolOptions) (db.DB, error) {
	primary, err := p.Connect(primaryDSN, opts)
	if err != nil {
		return nil, fmt.Errorf("mssql failover primary: %w", err)
	}
	var replicas []db.DB
	for i, dsn := range replicaDSNs {
		r, err := p.Connect(dsn, opts)
		if err != nil {
			for _, prev := range replicas {
				_ = prev.Close() // err suppressed: best-effort cleanup, replacing with new conn
			}
			_ = primary.Close() // err suppressed: best-effort cleanup
			return nil, fmt.Errorf("mssql failover replica[%d]: %w", i, err)
		}
		replicas = append(replicas, r)
	}
	return NewFailoverDB(FailoverConfig{
		Primary:       primary,
		Replicas:      replicas,
		CheckInterval: opts.HealthCheckPeriod,
	}), nil
}
