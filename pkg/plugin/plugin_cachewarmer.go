package plugin

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// CacheWarmer is an optional interface that plugins implement to participate
// in the startup cache warmup phase. After all plugins have started and the
// readiness probe passes, the activator calls WarmCache on every running
// plugin that implements this interface.
//
// WarmCache is called with a context that carries the global shutdown deadline
// budget. Plugins should respect context cancellation: if the budget expires
// before warmup completes, the plugin should return ctx.Err() so the
// activator can log which plugins didn't finish in time.
//
// WarmCache is called sequentially (not in parallel) so plugins that depend
// on shared resources (DB connections, query cache) don't contend. A plugin
// whose WarmCache returns an error does not prevent other plugins from
// warming: errors are logged individually.
//
//	func (p *MyPlugin) WarmCache(ctx context.Context) error {
//	    _, err := p.store.PreloadSchemas(ctx)
//	    return err
//	}
type CacheWarmer interface {
	core.Plugin

	// WarmCache preloads the plugin's caches. Called once after all plugins
	// have started and the readiness probe returns OK. Must respect context
	// cancellation (the shutdown signal may arrive during warmup).
	WarmCache(ctx context.Context) error
}

// WarmCacheAll iterates over running plugins and calls WarmCache on each one
// that implements CacheWarmer. Returns the total count of plugins that were
// warmed, and a combined error if any plugin failed. Individual failures are
// logged but do not stop the iteration.
func (a *Activator) WarmCacheAll(ctx context.Context) (warmed int, _ error) {
	a.mu.RLock()
	names := make([]string, 0, len(a.active))
	for name, ap := range a.active {
		if ap.phase == PhaseRunning && ap.plugin != nil {
			names = append(names, name)
		}
	}
	a.mu.RUnlock()

	var errs []string
	for _, name := range names {
		cw, ok := a.warmerPlugin(name)
		if !ok {
			continue
		}
		a.logger.Info("warming plugin cache", "plugin", name)
		if err := cw.WarmCache(ctx); err != nil {
			a.logger.Error("plugin cache warmup failed", "plugin", name, "err", err)
			if ctx.Err() != nil {
				errs = append(errs, name+": "+ctx.Err().Error())
			} else {
				errs = append(errs, name+": "+err.Error())
			}
		} else {
			warmed++
			a.logger.Debug("plugin cache warmup complete", "plugin", name)
		}
	}

	if len(errs) > 0 {
		return warmed, &warmErr{errs: errs}
	}
	return warmed, nil
}

// warmerPlugin returns the CacheWarmer interface for the named active plugin.
func (a *Activator) warmerPlugin(name string) (CacheWarmer, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	ap, ok := a.active[name]
	if !ok || ap.plugin == nil {
		return nil, false
	}
	cw, ok := ap.plugin.(CacheWarmer)
	return cw, ok
}

type warmErr struct{ errs []string }

func (e *warmErr) Error() string {
	s := "cache warmup errors: "
	for i, msg := range e.errs {
		if i > 0 {
			s += "; "
		}
		s += msg
	}
	return s
}
