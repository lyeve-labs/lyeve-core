// Package provider abstracts database engine-specific behavior into a unified
// Provider interface. Existing code continues to use db.DB + dialect.Dialect  --
// this package composes those into richer providers with capability detection,
// DSN parsing, and failover support.
package provider

import (
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
)

// Provider is the unified database engine abstraction. Each implementation
// bundles dialect, capabilities, DSN parsing, pool defaults for a single
// database engine.
type Provider interface {
	// Name returns the canonical engine name, same as Dialect.Name().
	Name() string

	// Dialect returns the SQL dialect for type mapping and DDL generation.
	Dialect() dialect.Dialect

	// Capabilities returns the feature flags this engine supports.
	Capabilities() Capabilities

	// Connect opens a new connection pool for the given DSN.
	Connect(dsn string, opts PoolOptions) (db.DB, error)

	// ConnectWithFailover opens a connection pool with replica failover.
	// When primary becomes unhealthy, reads route to the next healthy replica.
	ConnectWithFailover(primaryDSN string, replicaDSNs []string, opts PoolOptions) (db.DB, error)

	// DriverName returns the database/sql driver name for sql.Open.
	DriverName() string

	// DSNParser returns an engine-specific DSN parser.
	DSNParser() DSNParser

	// PoolDefaults returns recommended pool sizing for this engine.
	PoolDefaults() PoolDefaults
}
