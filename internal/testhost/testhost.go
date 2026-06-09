// Package testhost provides a lightweight core.Host backed by a
// testcontainers database for plugin lifecycle integration tests.
//
// It implements the full core.Host interface so plugins can exercise
// their Start->Migrate->CRUD->Stop cycle against real PostgreSQL, MySQL,
// and MSSQL containers.
package testhost

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
)

// Host wraps the production engineHost (via core.NewHost) with a
// zero-config Config and a no-op Hooks registry. Plugins that need
// more specific configuration can set fields on cfg after construction.
type Host struct {
	core.Host
	pool  db.DB
	rawDB *sql.DB
	cfg   *config.Config
}

// New creates a Host from a connected db.DB. The dialect is read from
// pool.Engine() and the raw *sql.DB is extracted via pool.SQLDB().
func New(pool db.DB) *Host {
	rawDB := pool.SQLDB()
	cfg := &config.Config{
		InstanceID:              "test",
		GracefulShutdownTimeout: 30 * time.Second,
	}
	h := &Host{
		pool:  pool,
		rawDB: rawDB,
		cfg:   cfg,
	}
	h.Host = enginehost.NewHost(pool, rawDB, cfg, nil, "test")
	return h
}

// Dialect returns the database dialect ("postgres", "mysql", "mssql").
func (h *Host) Dialect() string { return h.pool.Engine() }

var _ core.WriteSerializer = (*Host)(nil)

// SerializeWrite forwards to the engine host. The embedded interface hides
// it, and a host that answered without the lock would let a ceiling test
// pass against writes that were never serialized.
func (h *Host) SerializeWrite(ctx context.Context, key string, fn func(context.Context) error) error {
	return core.SerializeWrite(ctx, h.Host, key, fn)
}

// RawDB returns the underlying *sql.DB for migration runners.
func (h *Host) RawDB() *sql.DB { return h.rawDB }

// Querier returns a Querier scoped to the current context.
func (h *Host) Querier(ctx context.Context) core.Querier {
	return h.Host.Querier(ctx)
}

// Logger returns slog.Default().
func (h *Host) Logger(ctx context.Context) *slog.Logger {
	return slog.Default()
}

// Config returns the test configuration.
func (h *Host) Config() core.Config { return h.Host.Config() }

// Hooks returns the hook bus (nil when no registry is wired).
func (h *Host) Hooks() core.HookBus { return h.Host.Hooks() }

// Version returns "test".
func (h *Host) Version() string { return "test" }

// Schema returns nil: no schema engine is wired in the test host.
func (h *Host) Schema() core.SchemaEngine { return nil }

// Pool exposes the underlying connection pool for tests that need
// direct database access to verify plugin state.
func (h *Host) Pool() db.DB { return h.pool }

// Close shuts down the underlying pool.
func (h *Host) Close() error { return h.pool.Close() }

// EngineDBConn hands out a connection on the engine's own database.
//
// A generated content table is one table shared by every tenant, keyed by a
// tenant_id column, so whoever creates it has to reach the engine database
// whatever the caller's connection is bound to. Without this a schema applied
// from a tenant-scoped context would build a second copy inside that tenant's
// own schema.
func (h *Host) EngineDBConn(ctx context.Context) (*sql.Conn, error) {
	return h.pool.SQLDB().Conn(ctx)
}

var _ core.EngineDBConnProvider = (*Host)(nil)
