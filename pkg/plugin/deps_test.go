package plugin

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// TestResolvePluginDeps_SharedDependencyStartsFirst verifies that two plugins
// declaring a dependency on one plugin are placed at level 1, with the
// dependency at level 0, so a dependent never migrates before the tables of the
// plugin it depends on exist.
func TestResolvePluginDeps_SharedDependencyStartsFirst(t *testing.T) {
	defer resetPluginRegistry()

	// Register base (leaf: no deps).
	RegisterPlugin("base", func() core.Plugin {
		return &pluginWithDeps{name: "base"}
	})

	// Register alpha: depends on base.
	RegisterPlugin("alpha", func() core.Plugin {
		return &pluginWithDeps{name: "alpha", deps: []PluginDependency{{Name: "base"}}}
	})

	// Register beta: depends on base.
	RegisterPlugin("beta", func() core.Plugin {
		return &pluginWithDeps{name: "beta", deps: []PluginDependency{{Name: "base"}}}
	})

	res := ResolvePluginDeps([]string{"alpha", "beta", "base"})
	if res.HasErrors() {
		t.Fatalf("unexpected errors: missing=%v version=%v cycle=%v", res.Missing, res.Version, res.Cycle)
	}

	// base should be at level 0, alpha+beta at level 1.
	if len(res.Levels) != 2 {
		t.Fatalf("expected 2 levels, got %d: %v", len(res.Levels), res.Levels)
	}
	level0 := res.Levels[0]
	level1 := res.Levels[1]

	if !contains(level0, "base") {
		t.Errorf("expected base at level 0, got level 0=%v", level0)
	}
	if contains(level0, "alpha") || contains(level0, "beta") {
		t.Errorf("alpha/beta should NOT be at level 0: %v", level0)
	}
	if !contains(level1, "alpha") || !contains(level1, "beta") {
		t.Errorf("expected alpha+beta at level 1: %v", level1)
	}

	// Verify topological order: base before dependents.
	baseIdx := indexOf(res.Order, "base")
	alphaIdx := indexOf(res.Order, "alpha")
	betaIdx := indexOf(res.Order, "beta")
	if baseIdx >= alphaIdx {
		t.Errorf("base must precede alpha in order: order=%v", res.Order)
	}
	if baseIdx >= betaIdx {
		t.Errorf("base must precede beta in order: order=%v", res.Order)
	}

	t.Logf("order=%v levels=%v", res.Order, res.Levels)
}

// TestResolvePluginDeps_SingleDependency verifies a plugin with one dependency:
// the dependency sits at level 0 and the dependent at level 1.
func TestResolvePluginDeps_SingleDependency(t *testing.T) {
	defer resetPluginRegistry()

	RegisterPlugin("base", func() core.Plugin {
		return &pluginWithDeps{name: "base"}
	})
	RegisterPlugin("alpha", func() core.Plugin {
		return &pluginWithDeps{name: "alpha", deps: []PluginDependency{{Name: "base"}}}
	})

	res := ResolvePluginDeps([]string{"alpha", "base"})
	if res.HasErrors() {
		t.Fatalf("unexpected errors: missing=%v version=%v cycle=%v", res.Missing, res.Version, res.Cycle)
	}

	if len(res.Levels) != 2 {
		t.Fatalf("expected 2 levels, got %d: %v", len(res.Levels), res.Levels)
	}
	if !contains(res.Levels[0], "base") {
		t.Errorf("expected base at level 0: %v", res.Levels[0])
	}
	if !contains(res.Levels[1], "alpha") {
		t.Errorf("expected alpha at level 1: %v", res.Levels[1])
	}

	baseIdx := indexOf(res.Order, "base")
	alphaIdx := indexOf(res.Order, "alpha")
	if baseIdx >= alphaIdx {
		t.Errorf("base must precede alpha in order: order=%v", res.Order)
	}
}

// TestResolvePluginDeps_AutoAddDeps verifies that when only dependents are
// requested, their dependencies are auto-added and started first.
func TestResolvePluginDeps_AutoAddDeps(t *testing.T) {
	defer resetPluginRegistry()

	RegisterPlugin("base", func() core.Plugin {
		return &pluginWithDeps{name: "base"}
	})
	RegisterPlugin("alpha", func() core.Plugin {
		return &pluginWithDeps{name: "alpha", deps: []PluginDependency{{Name: "base"}}}
	})

	// Request only "alpha": base should be auto-added.
	res := ResolvePluginDeps([]string{"alpha"})
	if res.HasErrors() {
		t.Fatalf("unexpected errors: missing=%v", res.Missing)
	}
	if len(res.AutoAdded) != 1 || res.AutoAdded[0] != "base" {
		t.Errorf("expected 'base' auto-added, got %v", res.AutoAdded)
	}
	if len(res.Levels) != 2 {
		t.Fatalf("expected 2 levels, got %d: %v", len(res.Levels), res.Levels)
	}
	if !contains(res.Levels[0], "base") {
		t.Errorf("expected base at level 0: %v", res.Levels[0])
	}
	if !contains(res.Levels[1], "alpha") {
		t.Errorf("expected alpha at level 1: %v", res.Levels[1])
	}
}

// TestResolvePluginDeps_MissingDepError verifies that requesting a dependent
// without registering its dependency produces a Missing error.
func TestResolvePluginDeps_MissingDepError(t *testing.T) {
	defer resetPluginRegistry()

	RegisterPlugin("alpha", func() core.Plugin {
		return &pluginWithDeps{name: "alpha", deps: []PluginDependency{{Name: "base"}}}
	})

	res := ResolvePluginDeps([]string{"alpha"})
	if len(res.Missing) != 1 {
		t.Fatalf("expected 1 missing dep error, got %d: %v", len(res.Missing), res.Missing)
	}
	if res.Missing[0].Plugin != "alpha" || res.Missing[0].Dep != "base" {
		t.Errorf("wrong missing error: %+v", res.Missing[0])
	}
}

// pluginWithDeps is a minimal core.Plugin + Depender for testing.
type pluginWithDeps struct {
	name string
	deps []PluginDependency
}

func (p *pluginWithDeps) Name() string                                 { return p.name }
func (p *pluginWithDeps) Dependencies() []PluginDependency             { return p.deps }
func (p *pluginWithDeps) Start(ctx context.Context, h core.Host) error { return nil }
func (p *pluginWithDeps) Stop(ctx context.Context) error               { return nil }

var _ Depender = (*pluginWithDeps)(nil)
var _ core.Plugin = (*pluginWithDeps)(nil)

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestCompareVersion(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.4", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.2.3", "1.2.3", 0},
		{"v1.0.0", "1.0.0", 0},     // v-prefix stripped
		{"1.0.0-beta", "1.0.0", 0}, // pre-release stripped
		{"1.2", "1.2.0", 0},        // zero-padded (b longer)
		{"1.2.0", "1.2", 0},        // zero-padded (a longer)
		{"1.10.0", "1.9.0", 1},     // numeric, not lexical
	}
	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			if got := compareVersion(tt.a, tt.b); got != tt.want {
				t.Errorf("compareVersion(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
