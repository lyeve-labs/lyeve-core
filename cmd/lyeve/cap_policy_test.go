package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest/mockhost"
)

// migrationHost serves a migration handle, the way the engine's host serves
// its pool behind the scoped host.
type migrationHost struct {
	core.Host
	db *sql.DB
}

func (h migrationHost) MigrationDB() *sql.DB { return h.db }

// The goroutine engine keeps the runtime settings in a table of its own, so
// its scoped host serves the migration handle that creates the table and a
// querier that writes it. Without both, every write answers that nothing was
// stored.
func TestCapPolicy_GoroutineEngineReachesItsSettingsTable(t *testing.T) {
	plugin.SetCapPolicy(capPolicy)
	t.Cleanup(func() { plugin.SetCapPolicy(nil) })

	inner := migrationHost{Host: mockhost.New(t).Host(), db: new(sql.DB)}
	sh := core.NewScopedHost(inner, "goroutine-engine", plugin.CapPolicy("goroutine-engine"))
	if sh.MigrationDB() != inner.db {
		t.Error("goroutine-engine's scoped host withholds the migration handle")
	}
	ctx := context.Background()
	if _, err := sh.Querier(ctx).Exec(ctx, "UPDATE goroutine_engine_settings SET revision = revision + 1 WHERE id = 1"); errors.Is(err, core.ErrCapDenied) {
		t.Errorf("goroutine-engine's querier refused a write to its own table: %v", err)
	}
}
