package enginehost

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type stubFlowRegistry struct{}

func (stubFlowRegistry) Nodes() []core.FlowNode            { return nil }
func (stubFlowRegistry) Triggers() []core.FlowTrigger      { return nil }
func (stubFlowRegistry) OnChange(func()) core.Subscription { return nil }

// The host answers nil until the runtime wires a registry, and the very
// registry it was given afterwards, so the plugin that runs flows reads the
// activator's snapshot and not a copy that never changes.
func TestEngineHost_FlowRegistry_WiredByRuntime(t *testing.T) {
	h := &engineHost{}
	var host core.FlowRegistryHost = h
	if got := host.FlowRegistry(); got != nil {
		t.Fatalf("FlowRegistry() before wiring = %v, want nil", got)
	}
	reg := stubFlowRegistry{}
	h.WithFlowRegistry(reg)
	if got := host.FlowRegistry(); got != core.FlowRegistry(reg) {
		t.Errorf("FlowRegistry() = %v, want the wired registry", got)
	}
}
