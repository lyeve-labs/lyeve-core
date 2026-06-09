package plugintest

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// RunTenantPurges registers the purges p declares through core.TenantPurger,
// the way the engine does at boot, and runs them for slug in one transaction
// on host, as a tenant delete would. It returns the tables they cover.
//
// p should be a fresh instance that has never started: the engine takes its
// purges from one, so a handler that reads anything Start builds fails here
// first. The purge registries are cleared before and after, so the test
// sees only p's purges and leaves none behind.
func RunTenantPurges(t T, host core.Host, p core.Plugin, slug string) map[string]bool {
	t.Helper()
	purger, ok := p.(core.TenantPurger)
	if !ok {
		t.Fatalf("%s does not declare its tenant purges", p.Name())
		return nil
	}

	core.ResetTenantPurgeHandlers()
	core.ResetCoveredTables()
	defer core.ResetTenantPurgeHandlers()
	defer core.ResetCoveredTables()

	for _, tp := range purger.TenantPurges() {
		core.RegisterTenantPurge(host.Dialect(), tp)
	}
	covered := core.CoveredTableSet()

	ctx := context.Background()
	tx, err := host.Querier(ctx).Begin(ctx)
	if err != nil {
		t.Fatalf("begin the purge transaction: %v", err)
		return nil
	}
	for _, h := range core.TenantPurgeHandlers() {
		if err := h(ctx, tx, slug); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("tenant purge of %s: %v", p.Name(), err)
			return nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the purge transaction: %v", err)
		return nil
	}
	return covered
}
