package plugin

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type eraser struct{ calls int }

func (e *eraser) EraseSubject(context.Context, string) (int64, error) {
	e.calls++
	return 1, nil
}

// providerPlugin takes the documented route: it exposes its eraser rather than
// registering it during Start.
type providerPlugin struct {
	*fakePlugin
	e *eraser
}

func (p *providerPlugin) SubjectEraser() compliance.SubjectEraser { return p.e }

// TestRegisterSubjectErasers_ReachesTheProviderInterface pins the wiring the
// runtime calls after activation: a plugin that exposes its eraser through
// compliance.SubjectEraserProvider, instead of registering it, is reached by
// an erasure request exactly once.
func TestRegisterSubjectErasers_ReachesTheProviderInterface(t *testing.T) {
	t.Cleanup(resetPluginRegistry)

	provider := &providerPlugin{fakePlugin: newFakePlugin("alpha"), e: &eraser{}}
	plain := newFakePlugin("beta")

	RegisterPlugin("alpha", func() core.Plugin { return provider })
	RegisterPlugin("beta", func() core.Plugin { return plain })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("alpha", "beta"), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := a.RegisterSubjectErasers(); got != 1 {
		t.Fatalf("registered %d erasers; want the one plugin that exposes one", got)
	}

	// Calling twice is what a license change does. The eraser must not end up
	// in the registry a second time.
	if got := a.RegisterSubjectErasers(); got != 1 {
		t.Fatalf("second pass reported %d; want the same plugin", got)
	}

	n, err := compliance.RunSubjectErasure(context.Background(), "someone@example.com")
	if err != nil {
		t.Fatalf("RunSubjectErasure: %v", err)
	}
	if provider.e.calls != 1 {
		t.Errorf("eraser called %d times for one request; want once", provider.e.calls)
	}
	if n != 1 {
		t.Errorf("erasure reported %d rows; want 1", n)
	}
}
