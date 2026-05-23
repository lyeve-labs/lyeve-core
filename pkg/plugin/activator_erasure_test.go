package plugin

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// eraserPlugin declares one erasure over one table.
type eraserPlugin struct {
	*depPlugin
	table string
}

func (p *eraserPlugin) SubjectErasures() []compliance.SubjectErasure {
	return []compliance.SubjectErasure{{
		Tables: []string{p.table},
		Erase: func(context.Context, core.Host, string) (int64, error) {
			return 0, nil
		},
	}}
}

func resetEraserRegistry(t *testing.T) {
	t.Helper()
	compliance.ResetSubjectErasers()
	t.Cleanup(compliance.ResetSubjectErasers)
}

// A plugin the capability set refuses never starts, but it keeps the people
// an earlier run wrote, and an erasure has to reach them. Its erasure is
// registered all the same, once however often the pass runs, and named for
// the plugin.
func TestRegisterSubjectErasures_CoversAPluginThatDoesNotStart(t *testing.T) {
	t.Cleanup(resetPluginRegistry)
	resetEraserRegistry(t)

	refused := &eraserPlugin{depPlugin: &depPlugin{name: "refused"}, table: "refused_people"}
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

	if got := a.RegisterSubjectErasures(); got != 1 {
		t.Fatalf("registered %d plugins' erasures; want the one that declares one", got)
	}
	if got := a.RegisterSubjectErasures(); got != 0 {
		t.Fatalf("second pass registered %d; want none", got)
	}
	erasers := compliance.SubjectErasers()
	if len(erasers) != 1 {
		t.Fatalf("%d erasers registered; want 1", len(erasers))
	}
	if name := compliance.EraserName(erasers[0]); name != "refused" {
		t.Errorf("eraser named %q; want the declaring plugin", name)
	}
}

// A declared set with no function or no tables would register an eraser
// that can do nothing, so it is left out.
func TestRegisterSubjectErasures_SkipsAnEmptyDeclaration(t *testing.T) {
	t.Cleanup(resetPluginRegistry)
	resetEraserRegistry(t)

	empty := &emptyEraserPlugin{depPlugin: &depPlugin{name: "empty"}}
	RegisterPlugin("empty", func() core.Plugin { return empty })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants(), "")
	if got := a.RegisterSubjectErasures(); got != 0 {
		t.Fatalf("registered %d; want none", got)
	}
	if n := len(compliance.SubjectErasers()); n != 0 {
		t.Fatalf("%d erasers registered; want none", n)
	}
}

type emptyEraserPlugin struct{ *depPlugin }

func (p *emptyEraserPlugin) SubjectErasures() []compliance.SubjectErasure {
	return []compliance.SubjectErasure{
		{Tables: []string{"t"}},
		{Erase: func(context.Context, core.Host, string) (int64, error) { return 0, nil }},
	}
}

// A declared eraser reads and writes the database and reads a secret, and
// only where the plugin's own policy grants each: a plugin that encrypts a
// field needs its key to redact it, and an erasure is never a way to widen
// what the plugin may do.
func TestErasureHostCaps_NarrowsThePolicyToDatabaseAndSecrets(t *testing.T) {
	restoreCapTable(t)
	SetCapPolicy(map[string]core.Capability{
		"keyed":   core.CapDBRead | core.CapDBWrite | core.CapConfigSecret | core.CapRoutes | core.CapRawDB,
		"keyless": core.CapDBRead | core.CapDBWrite | core.CapRoutes,
	})
	RegisterPlugin("keyed", func() core.Plugin { return nil })
	defer UnregisterPlugin("keyed")
	RegisterPlugin("keyless", func() core.Plugin { return nil })
	defer UnregisterPlugin("keyless")

	if got, want := erasureHostCaps("keyed"), core.CapDBRead|core.CapDBWrite|core.CapConfigSecret; got != want {
		t.Errorf("erasureHostCaps(keyed) = %v, want %v", got, want)
	}
	if got, want := erasureHostCaps("keyless"), core.CapDBRead|core.CapDBWrite; got != want {
		t.Errorf("erasureHostCaps(keyless) = %v, want %v", got, want)
	}
}
