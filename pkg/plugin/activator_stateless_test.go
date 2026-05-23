package plugin

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// statelessPlugin is a depPlugin that answers core.StatelessCapable.
type statelessPlugin struct {
	*depPlugin
	capable bool
}

func (p *statelessPlugin) StatelessCapable() bool { return p.capable }

func TestActivator_StatelessStartsOnlyPluginsThatRunWithoutADatabase(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	// Every fixture is granted, so only the stateless capability decides
	// which of them starts.
	n := ungatedNames(t, 5)
	capable := &statelessPlugin{depPlugin: &depPlugin{name: n[0]}, capable: true}
	undeclared := &depPlugin{name: n[1]}
	declined := &statelessPlugin{depPlugin: &depPlugin{name: n[2]}, capable: false}
	// A capable plugin naming a peer that needs a database: the peer must
	// not start as its dependency either.
	capableWithDep := &statelessPlugin{depPlugin: &depPlugin{name: n[3], deps: []PluginDependency{{Name: n[4]}}}, capable: true}
	dep := &depPlugin{name: n[4]}

	for _, p := range []core.Plugin{capable, undeclared, declined, capableWithDep, dep} {
		RegisterPlugin(p.Name(), func() core.Plugin { return p })
	}

	a := NewActivator(testHost{}, nil)
	a.RequireStateless()
	a.Resolve(ungated(n...), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !capable.started || !capableWithDep.started {
		t.Errorf("stateless-capable plugins must start: %s=%v %s=%v", n[0], capable.started, n[3], capableWithDep.started)
	}
	if undeclared.started {
		t.Errorf("%s does not declare it runs without a database and must not start", n[1])
	}
	if declined.started {
		t.Errorf("%s declares false and must not start", n[2])
	}
	if dep.started {
		t.Errorf("%s needs a database and must not start as another plugin's dependency", n[4])
	}

	for _, s := range a.Status().Plugins {
		switch s.Name {
		case n[1], n[2], n[4]:
			if s.Active || s.Reason != needsDatabaseReason {
				t.Errorf("%s: active=%v reason=%q, want inactive with %q", s.Name, s.Active, s.Reason, needsDatabaseReason)
			}
		case n[0], n[3]:
			if !s.Active || s.Reason != "" {
				t.Errorf("%s: active=%v reason=%q, want running with no reason", s.Name, s.Active, s.Reason)
			}
		}
	}
}

func TestActivator_WithADatabaseTheCapabilityChangesNothing(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	n := ungatedNames(t, 2)
	undeclared := &depPlugin{name: n[0]}
	declined := &statelessPlugin{depPlugin: &depPlugin{name: n[1]}, capable: false}
	RegisterPlugin(n[0], func() core.Plugin { return undeclared })
	RegisterPlugin(n[1], func() core.Plugin { return declined })

	a := NewActivator(testHost{}, nil)
	a.Resolve(ungated(n...), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !undeclared.started || !declined.started {
		t.Errorf("an engine with a database starts every entitled plugin: %s=%v %s=%v", n[0], undeclared.started, n[1], declined.started)
	}
}
