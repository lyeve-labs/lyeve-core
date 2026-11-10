package main

import (
	"database/sql"
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

// The goroutine engine keeps the runtime settings in a table of its own and
// reads and writes it through the migration handle, so its scoped host serves
// that handle. Without it, every write answers that nothing was stored.
func TestCapPolicy_GoroutineEngineReachesItsSettingsTable(t *testing.T) {
	// The build's own table stays installed, as it does once the binary
	// boots. A reset to nil would take it from every later test in the
	// process that starts a plugin under it.
	plugin.SetCapPolicy(capPolicy)

	inner := migrationHost{Host: mockhost.New(t).Host(), db: new(sql.DB)}
	sh := core.NewScopedHost(inner, "goroutine-engine", plugin.CapPolicy("goroutine-engine"))
	if sh.MigrationDB() != inner.db {
		t.Error("goroutine-engine's scoped host withholds the migration handle")
	}
}
