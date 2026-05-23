package plugin

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// purgerPlugin owns one table and records every handler run in rec.
type purgerPlugin struct {
	*depPlugin
	table string
	rec   *[]string
}

func (p *purgerPlugin) TenantPurges() []core.TenantPurge {
	return []core.TenantPurge{{
		Tables: []string{p.table},
		Handler: func(context.Context, core.Querier, string) error {
			*p.rec = append(*p.rec, p.name)
			return nil
		},
	}}
}

func resetPurgeRegistries(t *testing.T) {
	t.Helper()
	core.ResetTenantPurgeHandlers()
	core.ResetCoveredTables()
	t.Cleanup(func() {
		core.ResetTenantPurgeHandlers()
		core.ResetCoveredTables()
	})
}

// A plugin the capability set refuses never starts, but it keeps the tables
// an earlier run created, and a tenant delete has to reach them. Its purge
// is registered all the same, once however often the pass runs.
func TestRegisterTenantPurges_CoversAPluginThatDoesNotStart(t *testing.T) {
	t.Cleanup(resetPluginRegistry)
	resetPurgeRegistries(t)

	var rec []string
	refused := &purgerPlugin{depPlugin: &depPlugin{name: "refused"}, table: "refused_rows", rec: &rec}
	RegisterPlugin("refused", func() core.Plugin { return refused })
	RegisterPlugin("plain", func() core.Plugin { return newFakePlugin("plain") })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("plain"), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if refused.started {
		t.Fatal("the refused plugin started")
	}

	if got := a.RegisterTenantPurges("postgres"); got != 1 {
		t.Fatalf("registered %d plugins' purges; want the one that declares a purge", got)
	}
	if got := a.RegisterTenantPurges("postgres"); got != 0 {
		t.Fatalf("second pass registered %d; want none", got)
	}
	if n := core.TenantPurgeHandlerCount(); n != 1 {
		t.Fatalf("%d handlers registered; want 1", n)
	}
	if !core.CoveredTableSet()["refused_rows"] {
		t.Error("the refused plugin's table is not covered")
	}
}

// A dependent's handler can find its rows through a table the plugin it
// depends on owns, so it runs first.
func TestRegisterTenantPurges_DependentsPurgeFirst(t *testing.T) {
	t.Cleanup(resetPluginRegistry)
	resetPurgeRegistries(t)

	var rec []string
	base := &purgerPlugin{depPlugin: &depPlugin{name: "base"}, table: "base_rows", rec: &rec}
	child := &purgerPlugin{
		depPlugin: &depPlugin{name: "child", deps: []PluginDependency{{Name: "base"}}},
		table:     "child_rows",
		rec:       &rec,
	}
	RegisterPlugin("base", func() core.Plugin { return base })
	RegisterPlugin("child", func() core.Plugin { return child })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants(), "")
	if got := a.RegisterTenantPurges("postgres"); got != 2 {
		t.Fatalf("registered %d plugins' purges; want 2", got)
	}

	order := purgeOrder(RegisteredPlugins())
	if len(order) != 2 || order[0] != "child" || order[1] != "base" {
		t.Fatalf("purge order %v; want [child base]", order)
	}
}
