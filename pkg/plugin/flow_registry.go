package plugin

import (
	"fmt"
	"runtime/debug"
	"sort"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// flowRegistry is the activator's view of the node and trigger types the
// running plugins contribute. It is built from started plugins only, so a
// plugin the license does not grant, or one that failed to boot, has no
// types in it. The plugin that runs flows reads it through
// core.FlowRegistryHost and never sees a type it cannot execute.
//
// The snapshot is replaced whole under the lock and never mutated in place,
// so a reader holds a consistent set for as long as it keeps the slice.
type flowRegistry struct {
	mu       sync.RWMutex
	nodes    []core.FlowNode
	triggers []core.FlowTrigger

	// subs is kept in registration order because subscribers are called in
	// it. A map would call them in a different order on every rebuild.
	subs   []flowSub
	nextID uint64
}

type flowSub struct {
	id uint64
	fn func()
}

var _ core.FlowRegistry = (*flowRegistry)(nil)

func newFlowRegistry() *flowRegistry { return &flowRegistry{} }

// Nodes returns the current node types. The slice is a copy. The nodes are
// the registry's own wrappers, which carry the plugin name on the spec.
func (r *flowRegistry) Nodes() []core.FlowNode {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]core.FlowNode(nil), r.nodes...)
}

// Triggers returns the current trigger types, as Nodes does.
func (r *flowRegistry) Triggers() []core.FlowTrigger {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]core.FlowTrigger(nil), r.triggers...)
}

// OnChange registers fn to run after every rebuild, in registration order,
// on the goroutine that rebuilt. Unsubscribe removes it. A call already in
// flight completes.
func (r *flowRegistry) OnChange(fn func()) core.Subscription {
	if fn == nil {
		return deniedFlowSubscription{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := r.nextID
	r.subs = append(r.subs, flowSub{id: id, fn: fn})
	return &flowSubscription{r: r, id: id}
}

// swap installs a new snapshot. It does not notify: the caller notifies once
// the lock is released, so a subscriber can read the registry from the call.
func (r *flowRegistry) swap(nodes []core.FlowNode, triggers []core.FlowTrigger) {
	r.mu.Lock()
	r.nodes = nodes
	r.triggers = triggers
	r.mu.Unlock()
}

// notify calls every subscriber in registration order, outside the lock.
func (r *flowRegistry) notify() {
	r.mu.RLock()
	subs := make([]flowSub, len(r.subs))
	copy(subs, r.subs)
	r.mu.RUnlock()
	for _, s := range subs {
		s.fn()
	}
}

type flowSubscription struct {
	r  *flowRegistry
	id uint64
}

func (s *flowSubscription) Unsubscribe() {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	for i, sub := range s.r.subs {
		if sub.id == s.id {
			s.r.subs = append(s.r.subs[:i:i], s.r.subs[i+1:]...)
			return
		}
	}
}

// deniedFlowSubscription is what a nil subscriber gets: nothing was
// registered, so there is nothing to remove.
type deniedFlowSubscription struct{}

func (deniedFlowSubscription) Unsubscribe() {}

// contributedNode is a provider's node with the spec the engine publishes:
// the provider's, with Plugin set to the contributing plugin's name. Spec is
// read once at registration and served from the copy, so a provider cannot
// change what it published after the registry accepted it.
type contributedNode struct {
	core.FlowNode
	spec core.FlowNodeSpec
}

func (n contributedNode) Spec() core.FlowNodeSpec { return n.spec }

type contributedTrigger struct {
	core.FlowTrigger
	spec core.FlowNodeSpec
}

func (t contributedTrigger) Spec() core.FlowNodeSpec { return t.spec }

// FlowRegistry returns the registry of node and trigger types the running
// plugins contribute. It exists from construction, empty, so the runtime can
// hand it to the engine host before any plugin starts. RebuildFlowRegistry
// fills it.
func (a *Activator) FlowRegistry() core.FlowRegistry {
	return a.flows
}

// RebuildFlowRegistry asks every running plugin for its flow nodes and
// triggers, installs the result as the registry's snapshot and calls the
// subscribers. It returns how many node and trigger types the snapshot holds.
//
// Start calls it once every plugin has started, OnDemandStart and
// ReloadPlugin call it when a plugin comes up later, and the runtime calls it
// from its license.changed handler, which runs after every plugin's own, so
// the snapshot follows the entitlement the plugins have just applied.
//
// A plugin whose contribution breaks a rule loses the whole contribution,
// nodes and triggers alike, and the engine keeps booting: one plugin
// publishing a type under another plugin's name, or twice, is that plugin's
// defect, and refusing it as a whole is what keeps a half-registered plugin
// from looking like a working one. The rules are the ones core.FlowNodeSpec
// states: a type carries the plugin's own prefix, a verb follows it, a type
// appears once, and a provider returns no nil entry.
//
// Subscribers run on the calling goroutine, after the snapshot is installed,
// one rebuild at a time. A subscriber must not call this method.
func (a *Activator) RebuildFlowRegistry() (nodes, triggers int) {
	a.flowRebuildMu.Lock()
	defer a.flowRebuildMu.Unlock()

	a.mu.RLock()
	names := make([]string, 0, len(a.active))
	instances := make(map[string]core.Plugin, len(a.active))
	for name, ap := range a.active {
		if ap == nil || ap.plugin == nil || ap.phase != PhaseRunning {
			continue
		}
		names = append(names, name)
		instances[name] = ap.plugin
	}
	a.mu.RUnlock()
	// Name order: the catalog a client sees must not change shape between
	// boots, and the map above iterates randomly.
	sort.Strings(names)

	var allNodes []core.FlowNode
	var allTriggers []core.FlowTrigger
	for _, name := range names {
		ns, ts, ok := a.collectFlowContribution(name, instances[name])
		if !ok {
			continue
		}
		allNodes = append(allNodes, ns...)
		allTriggers = append(allTriggers, ts...)
	}

	a.flows.swap(allNodes, allTriggers)
	a.flows.notify()
	return len(allNodes), len(allTriggers)
}

// collectFlowContribution reads one plugin's nodes and triggers and checks
// them against the naming rules. ok is false when the plugin contributes
// nothing, or when its contribution was refused. The refusal is logged with
// the type that caused it.
func (a *Activator) collectFlowContribution(name string, p core.Plugin) (nodes []core.FlowNode, triggers []core.FlowTrigger, ok bool) {
	np, isNodeProvider := p.(core.FlowNodeProvider)
	tp, isTriggerProvider := p.(core.FlowTriggerProvider)
	if !isNodeProvider && !isTriggerProvider {
		return nil, nil, false
	}

	var rawNodes []core.FlowNode
	var rawTriggers []core.FlowTrigger
	var err error
	if isNodeProvider {
		rawNodes, err = a.safeFlowNodes(name, np)
	}
	if err == nil && isTriggerProvider {
		rawTriggers, err = a.safeFlowTriggers(name, tp)
	}
	if err != nil {
		a.logger.Error("plugin flow contribution refused; none of its node or trigger types are registered",
			"plugin", name, "err", err)
		return nil, nil, false
	}

	seen := make(map[string]bool, len(rawNodes)+len(rawTriggers))
	accept := func(kind string, spec core.FlowNodeSpec) (core.FlowNodeSpec, error) {
		if !core.FlowTypeOwnedBy(name, spec.Type) {
			return spec, fmt.Errorf("%s type %q does not carry the plugin's prefix %q", kind, spec.Type, core.FlowTypePrefix(name))
		}
		if seen[spec.Type] {
			return spec, fmt.Errorf("%s type %q is contributed twice", kind, spec.Type)
		}
		seen[spec.Type] = true
		spec.Plugin = name
		return spec, nil
	}

	for _, n := range rawNodes {
		if n == nil {
			err = fmt.Errorf("FlowNodes returned a nil node")
			break
		}
		spec, aerr := accept("node", n.Spec())
		if aerr != nil {
			err = aerr
			break
		}
		nodes = append(nodes, contributedNode{FlowNode: n, spec: spec})
	}
	for _, t := range rawTriggers {
		if err != nil {
			break
		}
		if t == nil {
			err = fmt.Errorf("FlowTriggers returned a nil trigger")
			break
		}
		spec, aerr := accept("trigger", t.Spec())
		if aerr != nil {
			err = aerr
			break
		}
		triggers = append(triggers, contributedTrigger{FlowTrigger: t, spec: spec})
	}
	if err != nil {
		a.logger.Error("plugin flow contribution refused; none of its node or trigger types are registered",
			"plugin", name, "err", err)
		return nil, nil, false
	}
	return nodes, triggers, true
}

// safeFlowNodes calls a provider without letting a panic in it take the
// process down. A rebuild runs on a license change, on whatever goroutine
// published it, where nothing else recovers.
func (a *Activator) safeFlowNodes(name string, p core.FlowNodeProvider) (nodes []core.FlowNode, err error) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("plugin FlowNodes() panicked",
				"plugin", name, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			nodes, err = nil, fmt.Errorf("FlowNodes panicked: %v", r)
		}
	}()
	return p.FlowNodes(), nil
}

func (a *Activator) safeFlowTriggers(name string, p core.FlowTriggerProvider) (triggers []core.FlowTrigger, err error) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("plugin FlowTriggers() panicked",
				"plugin", name, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			triggers, err = nil, fmt.Errorf("FlowTriggers panicked: %v", r)
		}
	}()
	return p.FlowTriggers(), nil
}
