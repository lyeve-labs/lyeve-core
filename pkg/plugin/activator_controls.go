package plugin

import (
	"reflect"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// ReportControls asks every compiled plugin that implements
// core.SecurityControlReporter for its rows, in plugin name order. env holds
// what the runtime knows about the install. The activator fills in, per
// plugin, whether it runs and which of its roles the engine wired.
//
// A running plugin answers as it runs. A compiled plugin that does not run
// answers from a fresh instance nothing started, with Running false, so the
// controls it would supply read SKIP rather than vanish from the report.
func (a *Activator) ReportControls(env core.ControlEnv) []core.SecurityControl {
	running := a.running()
	byName := make(map[string]core.Plugin, len(running))
	for _, np := range running {
		byName[np.name] = np.plugin
	}

	a.mu.RLock()
	compiled := append([]string(nil), a.compiled...)
	a.mu.RUnlock()

	var out []core.SecurityControl
	for _, name := range compiled {
		p, isRunning := byName[name]
		if !isRunning {
			factory, ok := pluginFactory(name)
			if !ok {
				continue
			}
			p = factory()
		}
		reporter, ok := p.(core.SecurityControlReporter)
		if !ok {
			continue
		}
		e := env
		e.Running = isRunning
		e.Wired = map[string]bool{}
		if isRunning {
			e.Wired = wiredRolesOf(p, running)
		}
		out = append(out, reporter.SecurityControls(e)...)
	}
	return out
}

// wiredRolesOf says, for each role p implements, whether the engine wires p's
// implementation: always for a role any number of plugins fill, and for a
// single role only when p is its one running implementer.
func wiredRolesOf(p core.Plugin, running []namedPlugin) map[string]bool {
	wired := map[string]bool{}
	for _, r := range wiredRoles {
		if !reflect.TypeOf(p).Implements(r.Type) {
			continue
		}
		if !r.Single {
			wired[r.Name] = true
			continue
		}
		implementers := 0
		for _, np := range running {
			if reflect.TypeOf(np.plugin).Implements(r.Type) {
				implementers++
			}
		}
		wired[r.Name] = implementers == 1
	}
	return wired
}
