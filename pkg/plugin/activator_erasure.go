package plugin

import (
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// erasureCaps is what a declared eraser may do with the host it is handed:
// read and write the database, and read a secret. A plugin that encrypts a
// field at rest needs its key to find the subject in that field and to write
// the redacted value back encrypted, so without it the erasure would either
// miss the subject or store the rest of the field in the clear.
const erasureCaps = core.CapDBRead | core.CapDBWrite | core.CapConfigSecret

// erasureHostCaps is the grant a declared eraser of the named plugin runs
// with: what the plugin's policy grants, narrowed to erasureCaps.
func erasureHostCaps(name string) core.Capability {
	return CapPolicy(name) & erasureCaps
}

// RegisterSubjectErasures registers the subject erasures of every plugin
// compiled into the build, through compliance.SubjectEraserDeclarer, and
// reports how many plugins contributed one. It runs once per activator, and
// a later call registers nothing.
//
// Every compiled plugin counts, not only the running ones, for the reason
// RegisterTenantPurges gives: a plugin that does not start in this run still
// holds the rows an earlier run wrote, and an erasure has to reach them. Each
// erasure comes from a fresh instance and gets a host scoped to the
// database, because it must work on a plugin that never starts.
func (a *Activator) RegisterSubjectErasures() int {
	a.mu.Lock()
	if a.erasuresRegistered {
		a.mu.Unlock()
		return 0
	}
	a.erasuresRegistered = true
	a.mu.Unlock()

	registered := 0
	for _, name := range purgeOrder(RegisteredPlugins()) {
		factory, ok := pluginFactory(name)
		if !ok {
			continue
		}
		p, ok := factory().(compliance.SubjectEraserDeclarer)
		if !ok {
			continue
		}
		host := core.NewScopedHost(a.host, name, erasureHostCaps(name))
		sets := 0
		for _, se := range p.SubjectErasures() {
			if e := compliance.NewDeclaredEraser(name, host, se); e != nil {
				compliance.RegisterSubjectEraser(e)
				sets++
			}
		}
		if sets > 0 {
			registered++
			a.logger.Debug("subject erasure registered", "plugin", name, "sets", sets)
		}
	}
	return registered
}
