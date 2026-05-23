package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
)

// MySQLProvider implements Provider for MySQL 8+ and MariaDB 10.3+.
type MySQLProvider struct{}

// Name returns the canonical engine name "mysql" for MySQL.
func (p MySQLProvider) Name() string { return "mysql" }

// Dialect returns the MySQL dialect for type mapping and DDL generation.
func (p MySQLProvider) Dialect() dialect.Dialect { return dialect.MySQL{} }

// DriverName returns the database/sql driver name "mysql" for MySQL.
func (p MySQLProvider) DriverName() string { return "mysql" }

// DSNParser returns a MySQL-specific DSN parser for connection strings.
func (p MySQLProvider) DSNParser() DSNParser { return mysqlDSNParser{} }

// Capabilities returns the set of feature flags that MySQL 8+ supports.
func (p MySQLProvider) Capabilities() Capabilities {
	return NewCapabilities(
		CapCreateTableIfNotExists,
		// MySQL 8.0 lacks IF [NOT] EXISTS on ADD/DROP COLUMN: not advertised.
		CapCTE,
		CapCTERecursive,
		CapWindowFunc,
		CapFullText,
		CapUpsertOnDuplicate,
		CapAdvisoryLock,
		CapRowLock,
		CapSavepoint,
		// MySQL 8+ has native JSON, earlier versions not supported
		CapJSONNative,
		CapIndexInclude,
		CapPartitionTable,
	)
}

// PoolDefaults returns recommended pool sizing parameters for MySQL.
func (p MySQLProvider) PoolDefaults() PoolDefaults {
	return PoolDefaults{
		RecommendedMaxConns:        50,
		RecommendedMinConns:        5,
		RecommendedConnMaxLifetime: 30 * time.Minute,
		RecommendedConnMaxIdleTime: 5 * time.Minute,
		MaxConnLimit:               1000,
	}
}

// Connect opens a new MySQL connection pool for the given DSN.
func (p MySQLProvider) Connect(dsn string, opts PoolOptions) (db.DB, error) {
	opts = ApplyPoolOptions(opts, p.PoolDefaults())
	return db.ConnectWithOptions(context.Background(), dsn, db.ConnectOptions{
		MaxConns:          opts.MaxConns,
		MinConns:          opts.MinConns,
		ConnMaxLifetime:   opts.ConnMaxLifetime,
		ConnMaxIdleTime:   opts.ConnMaxIdleTime,
		HealthCheckPeriod: opts.HealthCheckPeriod,
	})
}

// ConnectWithFailover opens a MySQL connection pool with replica failover.
func (p MySQLProvider) ConnectWithFailover(primaryDSN string, replicaDSNs []string, opts PoolOptions) (db.DB, error) {
	primary, err := p.Connect(primaryDSN, opts)
	if err != nil {
		return nil, fmt.Errorf("mysql failover primary: %w", err)
	}
	var replicas []db.DB
	for i, dsn := range replicaDSNs {
		r, err := p.Connect(dsn, opts)
		if err != nil {
			for _, prev := range replicas {
				_ = prev.Close() // err suppressed: best-effort cleanup, replacing with new conn
			}
			_ = primary.Close() // err suppressed: best-effort cleanup
			return nil, fmt.Errorf("mysql failover replica[%d]: %w", i, err)
		}
		replicas = append(replicas, r)
	}
	return NewFailoverDB(FailoverConfig{
		Primary:       primary,
		Replicas:      replicas,
		CheckInterval: opts.HealthCheckPeriod,
	}), nil
}
