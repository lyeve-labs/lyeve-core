package plugin

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// fakeFlowNode is a node whose only content is its spec. Run and Validate
// are never reached here: the registry stamps the spec and forwards the rest.
type fakeFlowNode struct {
	spec core.FlowNodeSpec
}

func (n fakeFlowNode) Spec() core.FlowNodeSpec       { return n.spec }
func (n fakeFlowNode) Validate(map[string]any) error { return nil }
func (n fakeFlowNode) Run(context.Context, *core.FlowInvocation) (*core.FlowResult, error) {
	return &core.FlowResult{}, nil
}

type fakeFlowTrigger struct {
	spec core.FlowNodeSpec
}

func (t fakeFlowTrigger) Spec() core.FlowNodeSpec       { return t.spec }
func (t fakeFlowTrigger) Validate(map[string]any) error { return nil }
func (t fakeFlowTrigger) Subscribe(context.Context, string, map[string]any, func(map[string]any)) (func(), error) {
	return func() {}, nil
}

// flowProviderPlugin is shaped like a plugin that contributes to flows: it
// starts like any other and answers FlowNodes and FlowTriggers with whatever
// the test gave it. A provider that panics models a defective plugin.
type flowProviderPlugin struct {
	name     string
	nodes    []core.FlowNode
	triggers []core.FlowTrigger
	panics   bool
	lazy     bool
}

var (
	_ core.FlowNodeProvider    = (*flowProviderPlugin)(nil)
	_ core.FlowTriggerProvider = (*flowProviderPlugin)(nil)
)

func (p *flowProviderPlugin) Name() string                           { return p.name }
func (p *flowProviderPlugin) Start(context.Context, core.Host) error { return nil }
func (p *flowProviderPlugin) Stop(context.Context) error             { return nil }
func (p *flowProviderPlugin) IsLazy() bool                           { return p.lazy }
func (p *flowProviderPlugin) LazyRoutes() []RouteDecl                { return nil }
func (p *flowProviderPlugin) FlowTriggers() []core.FlowTrigger       { return p.triggers }
func (p *flowProviderPlugin) FlowNodes() []core.FlowNode {
	if p.panics {
		panic("provider defect")
	}
	return p.nodes
}

func node(typ string) core.FlowNode {
	return fakeFlowNode{spec: core.FlowNodeSpec{Type: typ, Plugin: "someone-else"}}
}

func trigger(typ string) core.FlowTrigger {
	return fakeFlowTrigger{spec: core.FlowNodeSpec{Type: typ, Trigger: true}}
}

// withFlowProviders registers the given providers for the test and returns
// an activator that has resolved a license naming every one of them.
func withFlowProviders(t *testing.T, logger *slog.Logger, providers ...*flowProviderPlugin) *Activator {
	t.Helper()
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	features := make([]string, 0, len(providers))
	for _, p := range providers {
		RegisterPlugin(p.name, func() core.Plugin { return p })
		features = append(features, p.name)
	}
	a := NewActivator(testHost{}, logger)
	a.Resolve(grants(features...), "")
	return a
}

func types(nodes []core.FlowNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Spec().Type
	}
	return out
}

func triggerTypes(triggers []core.FlowTrigger) []string {
	out := make([]string, len(triggers))
	for i, tr := range triggers {
		out[i] = tr.Spec().Type
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An install where nothing contributes has an empty registry, and the
// subscribers still hear that the build happened.
func TestFlowRegistry_EmptyWhenNothingContributes(t *testing.T) {
	withRegistered(t, "delta", "epsilon")
	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(grants("delta", "epsilon"), "")

	reg := a.FlowRegistry()
	if reg == nil {
		t.Fatal("the registry must exist before Start so a plugin can find it in its own Start")
	}
	calls := 0
	reg.OnChange(func() { calls++ })

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, tr := len(reg.Nodes()), len(reg.Triggers()); n != 0 || tr != 0 {
		t.Errorf("registry holds %d nodes and %d triggers; want none", n, tr)
	}
	if calls != 1 {
		t.Errorf("OnChange called %d times after Start; want 1", calls)
	}
}

// The registry is the union of what the started plugins contribute, in
// plugin name order, with Plugin set by the engine whatever the provider
// wrote there. A registered plugin the license does not name contributes
// nothing because it never started.
func TestFlowRegistry_BuiltFromStartedPluginsOnly(t *testing.T) {
	beta := &flowProviderPlugin{name: "beta",
		nodes:    []core.FlowNode{node("beta.submit")},
		triggers: []core.FlowTrigger{trigger("beta.submitted")}}
	alpha := &flowProviderPlugin{name: "alpha",
		nodes: []core.FlowNode{node("alpha.create"), node("alpha.update")}}
	a := withFlowProviders(t, silentLogger, beta, alpha)

	// Registered, entitled by nothing.
	unlicensed := &flowProviderPlugin{name: "gamma", nodes: []core.FlowNode{node("gamma.deliver")}}
	RegisterPlugin(unlicensed.name, func() core.Plugin { return unlicensed })
	a.Resolve(grants("beta", "alpha"), "")

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg := a.FlowRegistry()
	if got := types(reg.Nodes()); !equalStrings(got, []string{"alpha.create", "alpha.update", "beta.submit"}) {
		t.Errorf("nodes = %v", got)
	}
	if got := triggerTypes(reg.Triggers()); !equalStrings(got, []string{"beta.submitted"}) {
		t.Errorf("triggers = %v", got)
	}
	for _, n := range reg.Nodes() {
		want := strings.SplitN(n.Spec().Type, ".", 2)[0]
		if n.Spec().Plugin != want {
			t.Errorf("node %s carries Plugin %q; want %q, set by the engine", n.Spec().Type, n.Spec().Plugin, want)
		}
	}
	if tr := reg.Triggers()[0]; tr.Spec().Plugin != "beta" || !tr.Spec().Trigger {
		t.Errorf("trigger spec = %+v", tr.Spec())
	}
}

// One bad type costs the plugin its whole contribution, nodes and triggers,
// and nothing else: the other plugins' types stay, the plugin itself keeps
// running, and the log names the plugin and the type.
func TestFlowRegistry_RefusedProviderIsSkippedWholeAndBootContinues(t *testing.T) {
	cases := []struct {
		name     string
		provider *flowProviderPlugin
		logWant  string
	}{
		{
			name: "foreign prefix",
			provider: &flowProviderPlugin{name: "beta",
				nodes:    []core.FlowNode{node("beta.submit"), node("alpha.create")},
				triggers: []core.FlowTrigger{trigger("beta.submitted")}},
			logWant: "alpha.create",
		},
		{
			name: "hyphenated plugin name spelled with the hyphen",
			provider: &flowProviderPlugin{name: "alpha-beta",
				nodes: []core.FlowNode{node("alpha-beta.run")}},
			logWant: "alpha-beta.run",
		},
		{
			name: "type contributed twice",
			provider: &flowProviderPlugin{name: "beta",
				nodes:    []core.FlowNode{node("beta.submit")},
				triggers: []core.FlowTrigger{trigger("beta.submit")}},
			logWant: "contributed twice",
		},
		{
			name: "no verb after the prefix",
			provider: &flowProviderPlugin{name: "beta",
				nodes: []core.FlowNode{node("beta.")}},
			logWant: "does not carry the plugin's prefix",
		},
		{
			name: "nil node",
			provider: &flowProviderPlugin{name: "beta",
				nodes: []core.FlowNode{node("beta.submit"), nil}},
			logWant: "nil node",
		},
		{
			name:     "provider panics",
			provider: &flowProviderPlugin{name: "beta", panics: true, triggers: []core.FlowTrigger{trigger("beta.submitted")}},
			logWant:  "panicked",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			alpha := &flowProviderPlugin{name: "alpha", nodes: []core.FlowNode{node("alpha.create")}}
			a := withFlowProviders(t, logger, tc.provider, alpha)

			if err := a.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			reg := a.FlowRegistry()
			if got := types(reg.Nodes()); !equalStrings(got, []string{"alpha.create"}) {
				t.Errorf("nodes = %v; want only alpha's", got)
			}
			if got := reg.Triggers(); len(got) != 0 {
				t.Errorf("triggers = %v; a refused provider keeps none of its triggers either", triggerTypes(got))
			}
			for _, s := range a.Status().Plugins {
				if s.Name == tc.provider.name && s.Phase != PhaseRunning {
					t.Errorf("%s is %s; a refused contribution must not stop the plugin", s.Name, s.Phase)
				}
			}
			log := buf.String()
			if !strings.Contains(log, "flow contribution refused") || !strings.Contains(log, "plugin="+tc.provider.name) {
				t.Errorf("refusal not logged against the plugin:\n%s", log)
			}
			if !strings.Contains(log, tc.logWant) {
				t.Errorf("log does not name the cause %q:\n%s", tc.logWant, log)
			}
		})
	}
}

// A license change rebuilds the registry from what is running at that
// moment: a provider that has stopped is gone, one that has come back is in
// again, and the subscribers see the new snapshot from inside the call.
func TestFlowRegistry_RebuiltOnLicenseChange(t *testing.T) {
	beta := &flowProviderPlugin{name: "beta", nodes: []core.FlowNode{node("beta.submit")}}
	alpha := &flowProviderPlugin{name: "alpha", nodes: []core.FlowNode{node("alpha.create")},
		triggers: []core.FlowTrigger{trigger("alpha.done")}}
	a := withFlowProviders(t, silentLogger, beta, alpha)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg := a.FlowRegistry()

	var seen [][]string
	reg.OnChange(func() { seen = append(seen, types(reg.Nodes())) })

	// alpha loses its license and is stopped, the way Stop records it.
	a.recordPhase("alpha", PhaseStopping)
	a.recordStopped("alpha")
	if n, tr := a.RebuildFlowRegistry(); n != 1 || tr != 0 {
		t.Errorf("after alpha stopped: %d nodes, %d triggers; want 1 and 0", n, tr)
	}
	if got := types(reg.Nodes()); !equalStrings(got, []string{"beta.submit"}) {
		t.Errorf("nodes after the license lapsed = %v", got)
	}
	if len(reg.Triggers()) != 0 {
		t.Errorf("alpha's trigger survived its stop: %v", triggerTypes(reg.Triggers()))
	}

	// The license comes back and alpha is started again.
	a.recordRunning("alpha", alpha)
	if n, tr := a.RebuildFlowRegistry(); n != 2 || tr != 1 {
		t.Errorf("after alpha restarted: %d nodes, %d triggers; want 2 and 1", n, tr)
	}

	if len(seen) != 2 {
		t.Fatalf("OnChange called %d times across two rebuilds; want 2", len(seen))
	}
	if !equalStrings(seen[0], []string{"beta.submit"}) {
		t.Errorf("first notification saw %v; want the snapshot without alpha", seen[0])
	}
	if !equalStrings(seen[1], []string{"alpha.create", "beta.submit"}) {
		t.Errorf("second notification saw %v; want alpha back", seen[1])
	}
}

// A provider that starts after boot, on first use, is in the registry once
// it has started and not before.
func TestFlowRegistry_LazyProviderJoinsOnDemand(t *testing.T) {
	beta := &flowProviderPlugin{name: "beta", lazy: true, nodes: []core.FlowNode{node("beta.submit")}}
	a := withFlowProviders(t, silentLogger, beta)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg := a.FlowRegistry()
	if len(reg.Nodes()) != 0 {
		t.Fatalf("a lazy plugin has not started; its types must not be registered: %v", types(reg.Nodes()))
	}
	calls := 0
	reg.OnChange(func() { calls++ })
	if err := a.OnDemandStart(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	if got := types(reg.Nodes()); !equalStrings(got, []string{"beta.submit"}) {
		t.Errorf("nodes after on-demand start = %v", got)
	}
	if calls != 1 {
		t.Errorf("OnChange called %d times by the on-demand start; want 1", calls)
	}
}

// Subscribers run in the order they subscribed, on the rebuilding goroutine,
// and one that unsubscribed is not called again.
func TestFlowRegistry_OnChangeOrderAndUnsubscribe(t *testing.T) {
	a := withFlowProviders(t, silentLogger)
	reg := a.FlowRegistry()

	var mu sync.Mutex
	var order []string
	record := func(name string) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
		}
	}
	reg.OnChange(record("first"))
	second := reg.OnChange(record("second"))
	reg.OnChange(record("third"))
	if sub := reg.OnChange(nil); sub == nil {
		t.Fatal("a nil subscriber still gets a subscription to discard")
	}

	a.RebuildFlowRegistry()
	if !equalStrings(order, []string{"first", "second", "third"}) {
		t.Fatalf("first rebuild called %v; want registration order", order)
	}

	second.Unsubscribe()
	second.Unsubscribe()
	order = nil
	a.RebuildFlowRegistry()
	if !equalStrings(order, []string{"first", "third"}) {
		t.Errorf("after unsubscribing second, rebuild called %v", order)
	}
}

// The slice a reader holds is its own: a rebuild does not change it under
// the reader, and a reader cannot change the registry through it.
func TestFlowRegistry_SnapshotsAreCopies(t *testing.T) {
	beta := &flowProviderPlugin{name: "beta", nodes: []core.FlowNode{node("beta.submit")}}
	a := withFlowProviders(t, silentLogger, beta)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg := a.FlowRegistry()
	held := reg.Nodes()
	held[0] = nil
	if got := reg.Nodes(); len(got) != 1 || got[0] == nil {
		t.Error("writing into a returned slice reached the registry")
	}
}
