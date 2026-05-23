package core

import (
	"testing"
)

func TestFlowTypePrefix_HyphensBecomeUnderscores(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"widgets":          "widgets.",
		"my-plugin":        "my_plugin.",
		"long-plugin-name": "long_plugin_name.",
	}
	for plugin, want := range cases {
		if got := FlowTypePrefix(plugin); got != want {
			t.Errorf("FlowTypePrefix(%q) = %q, want %q", plugin, got, want)
		}
	}
}

func TestFlowTypeOwnedBy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		plugin, typ string
		want        bool
	}{
		{"widgets", "widgets.submit", true},
		{"my-plugin", "my_plugin.verb", true},
		{"my-plugin", "my-plugin.verb", false},
		{"widgets", "other.verb", false},
		{"widgets", "widgets.", false},
		{"widgets", "widgetsx.submit", false},
		{"widgets", "", false},
	}
	for _, c := range cases {
		if got := FlowTypeOwnedBy(c.plugin, c.typ); got != c.want {
			t.Errorf("FlowTypeOwnedBy(%q, %q) = %v, want %v", c.plugin, c.typ, got, c.want)
		}
	}
}

// flowHost is a stubHost that also carries a registry, the way the engine
// host does once the runtime wires one.
type flowHost struct {
	*stubHost
	reg FlowRegistry
}

func (h *flowHost) FlowRegistry() FlowRegistry { return h.reg }

type fixedRegistry struct {
	nodes []FlowNode
}

func (r *fixedRegistry) Nodes() []FlowNode            { return r.nodes }
func (r *fixedRegistry) Triggers() []FlowTrigger      { return nil }
func (r *fixedRegistry) OnChange(func()) Subscription { return deniedSubscription{} }

type namedNode struct {
	FlowNode
	typ string
}

func (n namedNode) Spec() FlowNodeSpec { return FlowNodeSpec{Type: n.typ} }

// A granted plugin reads the registry the inner host holds, unwrapped.
func TestScopedHost_FlowRegistry_ForwardsWhenGranted(t *testing.T) {
	reg := &fixedRegistry{nodes: []FlowNode{namedNode{typ: "widgets.submit"}}}
	sh := NewScopedHost(&flowHost{stubHost: &stubHost{}, reg: reg}, "flow", CapFlowRegistry)

	var host Host = sh
	fr, ok := host.(FlowRegistryHost)
	if !ok {
		t.Fatal("ScopedHost must satisfy FlowRegistryHost through the Host interface")
	}
	got := fr.FlowRegistry()
	if got != FlowRegistry(reg) {
		t.Fatalf("FlowRegistry() = %T, want the inner host's registry", got)
	}
	if len(got.Nodes()) != 1 || got.Nodes()[0].Spec().Type != "widgets.submit" {
		t.Errorf("registry contents did not pass through: %+v", got.Nodes())
	}
}

// Without the grant the plugin gets a registry that contributes nothing,
// not the real one, and the denial is in the log with the grant to add.
func TestScopedHost_FlowRegistry_DeniedIsEmptyAndLogged(t *testing.T) {
	h := newCapturingHost()
	reg := &fixedRegistry{nodes: []FlowNode{namedNode{typ: "widgets.submit"}}}
	sh := NewScopedHost(&capturingFlowHost{capturingHost: h, reg: reg}, "flow", CapDBRead|CapRoutes)

	got := sh.FlowRegistry()
	if got == nil {
		t.Fatal("a denied FlowRegistry must be a stub, not nil")
	}
	if len(got.Nodes()) != 0 || len(got.Triggers()) != 0 {
		t.Errorf("denied registry leaked contributions: %d nodes, %d triggers", len(got.Nodes()), len(got.Triggers()))
	}
	sub := got.OnChange(func() { t.Error("a denied registry never changes") })
	if sub == nil {
		t.Fatal("OnChange on a denied registry must return a subscription")
	}
	sub.Unsubscribe()
	sh.FlowRegistry()

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "FlowRegistry")
	assertField(t, rec, "capability", "CapFlowRegistry")
	assertField(t, rec, "plugin", "flow")
}

type capturingFlowHost struct {
	*capturingHost
	reg FlowRegistry
}

func (h *capturingFlowHost) FlowRegistry() FlowRegistry { return h.reg }

// A host that never had a registry wired answers nil, so a plugin can tell
// "no engine registry here" from "denied".
func TestScopedHost_FlowRegistry_NilWhenInnerHasNone(t *testing.T) {
	sh := NewScopedHost(&stubHost{}, "flow", CapAll)
	if got := sh.FlowRegistry(); got != nil {
		t.Errorf("FlowRegistry() = %v, want nil when the inner host has no registry", got)
	}
}

func TestCapAll_GrantsFlowRegistry(t *testing.T) {
	t.Parallel()
	if !CapAll.Has(CapFlowRegistry) {
		t.Error("CapAll must include CapFlowRegistry")
	}
	if capName(CapFlowRegistry) != "CapFlowRegistry" {
		t.Errorf("capName = %q", capName(CapFlowRegistry))
	}
}
