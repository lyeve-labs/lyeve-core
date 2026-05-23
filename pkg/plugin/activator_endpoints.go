package plugin

import (
	"context"
	"log/slog"
	"net/http"
	"sort"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The activator answers both custom endpoint roles itself, by asking the
// plugins running at the moment of the call. The routers hold the activator
// and not a plugin, so a plugin started on demand, reloaded or stopped after
// the routers were built is seen as it is now rather than as it was at boot.

var (
	_ core.CustomRouteProvider = (*Activator)(nil)
	_ core.EndpointDocumenter  = (*Activator)(nil)
)

// runningNames lists the active plugins in name order, so two providers
// claiming one path resolve the same way on every request and every
// replica. The caller holds a.mu.
func (a *Activator) runningNames() []string {
	names := make([]string, 0, len(a.active))
	for name, ap := range a.active {
		if ap != nil && ap.plugin != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// CustomRoute returns the first running plugin's handler for a request no
// registered route matched, or nil.
func (a *Activator) CustomRoute(r *http.Request) http.Handler {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, name := range a.runningNames() {
		if p, ok := a.active[name].plugin.(core.CustomRouteProvider); ok {
			if h := p.CustomRoute(r); h != nil {
				return h
			}
		}
	}
	return nil
}

// DocumentedEndpoints gathers the caller's tenant's endpoints from every
// running plugin that lists them. A plugin that fails is logged and left
// out, so one plugin's store outage does not take the reference down.
func (a *Activator) DocumentedEndpoints(ctx context.Context) ([]core.DocumentedEndpoint, error) {
	a.mu.RLock()
	var docs []struct {
		name string
		d    core.EndpointDocumenter
	}
	for _, name := range a.runningNames() {
		if d, ok := a.active[name].plugin.(core.EndpointDocumenter); ok {
			docs = append(docs, struct {
				name string
				d    core.EndpointDocumenter
			}{name, d})
		}
	}
	a.mu.RUnlock()

	var out []core.DocumentedEndpoint
	for _, e := range docs {
		eps, err := e.d.DocumentedEndpoints(ctx)
		if err != nil {
			slog.WarnContext(ctx, "plugin endpoints left out of the API reference", "plugin", e.name, "error", err)
			continue
		}
		out = append(out, eps...)
	}
	return out, nil
}
