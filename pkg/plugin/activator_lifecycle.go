package plugin

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
)

// CollectedRoutes returns the HTTP route declarations from all running
// plugins that implement RoutesPlugin. Safe to call from any goroutine.
// Takes a read lock. The returned slice is a copy.
func (a *Activator) CollectedRoutes() []PluginRoutes {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]PluginRoutes, len(a.routes))
	copy(out, a.routes)
	return out
}

// RecollectRoutes re-reads Routes() from every running plugin that declares
// them, and returns the fresh table.
//
// A license arriving after boot rebuilds a plugin's handler. The route the
// router mounted holds a method value bound to the handler that existed when
// routes were first collected, so without this the plugin activates and every
// one of its endpoints goes on answering from the stub it had while degraded.
// Re-reading is what produces a method value bound to the new handler.
//
// It re-reads rather than reusing what a plugin returned before, because that
// is the whole point: the same call now closes over something else.
func (a *Activator) RecollectRoutes() []PluginRoutes {
	a.mu.RLock()
	names := make([]string, 0, len(a.routes))
	for _, pr := range a.routes {
		names = append(names, pr.Name)
	}
	instances := make(map[string]core.Plugin, len(names))
	for _, n := range names {
		// Running only. A plugin that has been stopped normally has its routes
		// stripped too, but the two are separate maps, and asking a torn-down
		// plugin to build a route table is not worth the ordering assumption.
		if ap := a.active[n]; ap != nil && ap.phase == PhaseRunning {
			instances[n] = ap.plugin
		}
	}
	a.mu.RUnlock()

	fresh := make([]PluginRoutes, 0, len(names))
	for _, n := range names {
		rp, ok := instances[n].(RoutesPlugin)
		if !ok {
			continue
		}
		routes := a.safeRoutes(n, rp)
		if routes == nil {
			continue
		}
		fresh = append(fresh, PluginRoutes{Name: n, Routes: routes})
	}

	a.mu.Lock()
	for _, pr := range fresh {
		for i := range a.routes {
			if a.routes[i].Name == pr.Name {
				a.routes[i].Routes = pr.Routes
				break
			}
		}
	}
	a.mu.Unlock()

	// Always notify. Whether a handler actually moved cannot be tested from
	// here: Routes() builds a fresh closure per call, Go func values are not
	// comparable, and a method value's code pointer is the same whatever
	// receiver it carries. A license change is an operator action rather than
	// a poll, so rebuilding two routers on each one is the cheaper mistake.
	a.notifyRoutesChanged()
	return fresh
}

// safeRoutes calls a plugin's Routes() without letting a panic in it take the
// process down. Routes() runs at boot inside the activator's own recovery. This
// call happens on a license change, on whatever goroutine published it.
func (a *Activator) safeRoutes(name string, rp RoutesPlugin) (routes []RouteDecl) {
	defer func() {
		if r := recover(); r != nil {
			a.logger.Error("plugin Routes() panicked while re-collecting after a license change",
				"plugin", name, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			routes = nil
		}
	}()
	return rp.Routes()
}

// ReloadPlugin stops a running plugin, creates a fresh instance from its
// current factory in the registry, starts it, and updates the route table.
// Returns the new routes (nil if plugin implements no RoutesPlugin).
//
// The Reload is atomic from the caller's perspective: Stop -> instantiate -> Start.
// If Start fails, the previous instance is NOT restored (it was already stopped).
// The error is logged and the plugin is marked PhaseFailed. The caller receives
// the error and nil routes.
//
// Designed for dev-mode hot-reload. Not called in production code paths.
func (a *Activator) ReloadPlugin(ctx context.Context, name string) ([]RouteDecl, error) {
	// Phase 1: Stop the running instance.
	{
		// plugin and phase are read under the lock rather than off the
		// pointer afterwards. recordRunning writes both while holding it, so
		// reading them unlocked is a data race with any concurrent start of
		// the same plugin, which is exactly what a reload races against.
		a.mu.RLock()
		var (
			running core.Plugin
			phase   PluginPhase
		)
		if ap := a.active[name]; ap != nil {
			running, phase = ap.plugin, ap.phase
		}
		a.mu.RUnlock()

		if running != nil && phase == PhaseRunning {
			a.recordPhase(name, PhaseStopping)
			a.logger.Info("hot-reload: stopping plugin", "plugin", name)
			if err := running.Stop(ctx); err != nil {
				a.logger.Error("plugin stop during hot-reload returned error", "plugin", name, "err", err)
			}
			a.recordStopped(name)
		}

		// Remove old routes owned by this plugin.
		a.removeRoutes(name)
	}

	// Phase 2: Instantiate from the (potentially swapped) factory.
	factory, ok := pluginFactory(name)
	if !ok {
		err := fmt.Errorf("plugin %q not found in registry after factory swap", name)
		a.recordFailure(name, err)
		return nil, err
	}

	p := factory()
	a.recordPhase(name, PhaseStarting)
	a.logger.Info("hot-reload: starting plugin", "plugin", name)
	caps := CapPolicy(name)
	scoped := core.NewScopedHost(a.host, name, caps)
	if err := safeStart(ctx, p, scoped); err != nil {
		a.logger.Error("plugin start during hot-reload failed", "plugin", name, "err", err)
		a.recordFailure(name, err)
		return nil, err
	}
	a.recordRunning(name, p)
	a.logger.Info("hot-reload: plugin started", "plugin", name)

	// Phase 3: Collect routes.
	var routes []RouteDecl
	if rp, ok := p.(RoutesPlugin); ok {
		routes = rp.Routes()
	}

	if len(routes) > 0 {
		// collectRoutes handles conflict checking.
		if err := a.collectRoutes(name, routes); err != nil {
			a.logger.Error("plugin route collection during hot-reload failed",
				"plugin", name, "err", err)
			if stopErr := p.Stop(ctx); stopErr != nil {
				a.logger.Error("plugin stop after route conflict failed",
					"plugin", name, "err", stopErr)
			}
			a.recordFailure(name, fmt.Errorf("route conflict: %w", err))
			return nil, err
		}
	}

	// Phase 4: Notify route change listener.
	a.notifyRoutesChanged()
	a.RebuildFlowRegistry()

	return routes, nil
}

// removeRoutes strips all route declarations belonging to name from the
// collected routes slice. Locks a.mu itself.
func (a *Activator) removeRoutes(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	filtered := a.routes[:0]
	for _, pr := range a.routes {
		if pr.Name != name {
			filtered = append(filtered, pr)
		}
	}
	a.routes = filtered
}

// SetRoutesChangeCallback registers a function to call whenever the plugin
// route table changes (after Start, ReloadPlugin, or Stop). The runtime
// wires this to rebuild the chi router with the updated route set.
// Passing nil clears the callback.
// Only one callback is supported. Calling again replaces the previous one.
func (a *Activator) SetRoutesChangeCallback(fn func(routes []PluginRoutes)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onRoutesChanged = fn
}

// notifyRoutesChanged calls the registered callback with a snapshot of
// the current routes. Safe to call from any goroutine. The callback is
// invoked OUTSIDE the activator lock so it can safely call back into the
// activator (e.g., activator.MFAStore() takes a read lock).
func (a *Activator) notifyRoutesChanged() {
	a.mu.Lock()
	fn := a.onRoutesChanged
	if fn == nil {
		a.mu.Unlock()
		return
	}
	// Snapshot to avoid the callback mutating the slice.
	snap := make([]PluginRoutes, len(a.routes))
	copy(snap, a.routes)
	a.mu.Unlock()
	fn(snap)
}

// Stop drains every active plugin in reverse start order. A plugin's Stop
// error is logged but does not abort the shutdown of the remaining plugins.
func (a *Activator) Stop(ctx context.Context) error {
	a.mu.Lock()
	plugins := make([]string, 0, len(a.active))
	for name := range a.active {
		plugins = append(plugins, name)
	}
	a.mu.Unlock()

	// Reverse order: last-started is first-stopped, mirroring typical
	// startup-dependency patterns.
	sort.Sort(sort.Reverse(sort.StringSlice(plugins)))

	for _, name := range plugins {
		a.mu.RLock()
		ap := a.active[name]
		a.mu.RUnlock()
		if ap == nil || ap.plugin == nil {
			continue
		}
		a.recordPhase(name, PhaseStopping)
		a.logger.Info("stopping plugin", "plugin", name)
		if err := safeStop(ctx, ap.plugin); err != nil {
			a.logger.Error("plugin stop returned error", "plugin", name, "err", err)
		}
		a.recordStopped(name)
	}
	return nil
}

// safeStart runs a plugin's Start with panic recovery, so one plugin's panic
// becomes a failure for that plugin rather than a crash of the whole engine.
func safeStart(ctx context.Context, p core.Plugin, host core.Host) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Start: %v\n%s", r, debug.Stack())
		}
	}()
	return p.Start(ctx, host)
}

// safeStop runs a plugin's Stop with the same panic recovery as safeStart.
func safeStop(ctx context.Context, p core.Plugin) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Stop: %v\n%s", r, debug.Stack())
		}
	}()
	return p.Stop(ctx)
}

// WarmCacheAllParallel fans out cache warmup across running plugins using the
// provided ParallelEngine for bounded concurrency. Returns the count of plugins
// that were successfully warmed. Individual failures are logged but do not stop
// the iteration.
func (a *Activator) WarmCacheAllParallel(ctx context.Context, engine *engine.ParallelEngine) (int, error) {
	a.mu.RLock()
	names := make([]string, 0, len(a.active))
	for name := range a.active {
		names = append(names, name)
	}
	a.mu.RUnlock()

	var warmed atomic.Int32
	items := make([]any, len(names))
	for i, n := range names {
		items[i] = n
	}

	engine.FanOutVoid(ctx, items, func(taskCtx context.Context, item any, idx int) error {
		name := item.(string)
		cw, ok := a.warmerPlugin(name)
		if !ok {
			return nil
		}
		a.logger.Info("warming plugin cache", "plugin", name)
		if err := cw.WarmCache(taskCtx); err != nil {
			a.logger.Warn("plugin cache warmup failed", "plugin", name, "err", err)
			return err
		}
		warmed.Add(1)
		return nil
	})
	return int(warmed.Load()), nil
}

// StopParallel stops every active plugin in reverse start order with bounded
// parallelism via the provided ParallelEngine. Each plugin gets a 5-second
// deadline for stopping. Individual stop errors are logged and collected.
// The first error is returned after all plugins have been drained.
func (a *Activator) StopParallel(ctx context.Context, engine *engine.ParallelEngine) error {
	a.mu.Lock()
	names := make([]string, 0, len(a.active))
	for name := range a.active {
		names = append(names, name)
	}
	// Reverse order for stop (dependencies).
	for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
		names[i], names[j] = names[j], names[i]
	}
	a.mu.Unlock()

	items := make([]any, len(names))
	for i, n := range names {
		items[i] = n
	}

	var mu sync.Mutex
	var errs []error

	engine.FanOutVoid(ctx, items, func(taskCtx context.Context, item any, idx int) error {
		name := item.(string)
		a.mu.RLock()
		ap, ok := a.active[name]
		a.mu.RUnlock()
		if !ok || ap == nil || ap.plugin == nil {
			return nil
		}

		stopCtx, cancel := context.WithTimeout(taskCtx, 5*time.Second)
		defer cancel()

		a.logger.Info("stopping plugin", "name", name)
		if err := safeStop(stopCtx, ap.plugin); err != nil {
			a.logger.Error("plugin stop error", "name", name, "err", err)
			mu.Lock()
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			mu.Unlock()
		}
		return nil
	})

	if len(errs) > 0 {
		return fmt.Errorf("plugin stop: %d errors: %w", len(errs), errs[0])
	}
	return nil
}

// PluginByName returns the running plugin instance for name, or nil if
// inactive. Used by the runtime to wire capability interfaces between
// plugins after activation.
func (a *Activator) PluginByName(name string) core.Plugin {
	a.mu.RLock()
	defer a.mu.RUnlock()
	ap, ok := a.active[name]
	if !ok || ap.plugin == nil {
		return nil
	}
	return ap.plugin
}

// WireAuthCache hands backend to every active plugin that keeps lockout or
// block state, and reports how many took it. Called once after activation.
//
// Those plugins cannot reach the cache themselves: the backend is owned by
// another plugin and resolved by the runtime, and Host exposes no accessor for
// it.
func (a *Activator) WireAuthCache(backend core.CacheBackend) int {
	if backend == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	wired := 0
	for _, ap := range a.active {
		if ap.plugin == nil {
			continue
		}
		if c, ok := ap.plugin.(core.AuthCacheConsumer); ok {
			c.SetRateLimiterCache(backend)
			wired++
		}
	}
	return wired
}

// RegisterSubjectErasers registers the eraser of every active plugin that
// exposes one through compliance.SubjectEraserProvider, and reports how many
// were taken. Called once after activation.
//
// Without this, a plugin that implements the interface instead of registering
// would never be reached by POST /api/admin/gdpr/erase. Registering twice is
// safe: the registry ignores an eraser it already holds.
func (a *Activator) RegisterSubjectErasers() int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	registered := 0
	for _, ap := range a.active {
		if ap.plugin == nil {
			continue
		}
		p, ok := ap.plugin.(compliance.SubjectEraserProvider)
		if !ok {
			continue
		}
		if e := p.SubjectEraser(); e != nil {
			compliance.RegisterSubjectEraser(e)
			registered++
		}
	}
	return registered
}

// RegisterSubjectExporters registers the exporter of every active plugin that
// exposes one through compliance.SubjectExporterProvider, and reports how
// many were taken. Called once after activation, beside
// RegisterSubjectErasers.
//
// Without this, a plugin that implements the interface instead of registering
// would be absent from every export, and the export would still read as
// complete. Registering twice is safe: the registry ignores an exporter it
// already holds.
func (a *Activator) RegisterSubjectExporters() int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	registered := 0
	for _, ap := range a.active {
		if ap.plugin == nil {
			continue
		}
		p, ok := ap.plugin.(compliance.SubjectExporterProvider)
		if !ok {
			continue
		}
		if e := p.SubjectExporter(); e != nil {
			compliance.RegisterSubjectExporter(e)
			registered++
		}
	}
	return registered
}

// RegisterCustomValidators installs the schema validators of every active
// plugin that supplies any, and reports how many names were claimed. Called
// once after activation, beside RegisterSubjectErasers.
//
// A name no plugin claims is not an error: the schema engine falls back to
// reading the entry's body as a pattern, so an install with no plugin still
// validates. This exists so a plugin can give a name meaning that a pattern
// cannot express.
func (a *Activator) RegisterCustomValidators() int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	registered := 0
	for _, ap := range a.active {
		if ap.plugin == nil {
			continue
		}
		p, ok := ap.plugin.(core.CustomValidatorProvider)
		if !ok {
			continue
		}
		for name, fn := range p.CustomValidators() {
			if name == "" || fn == nil {
				continue
			}
			core.RegisterCustomValidator(name, fn)
			registered++
		}
	}
	return registered
}

// RegisterHoldChecker installs the legal-hold checker of the active plugin
// that exposes one, and reports whether it found one. Called once after
// activation, beside RegisterSubjectErasers.
//
// Holds live in a plugin, so on an install without one there is no checker
// and no hold that could exist. Erasure treats that as "free to erase". It
// treats a checker that fails as "nobody knows", and refuses. The two cases
// have to stay distinguishable, which is why this returns whether a checker
// was found rather than silently leaving nil behind.
func (a *Activator) RegisterHoldChecker() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	for _, ap := range a.active {
		if ap.plugin == nil {
			continue
		}
		p, ok := ap.plugin.(compliance.HoldCheckerProvider)
		if !ok {
			continue
		}
		if c := p.HoldChecker(); c != nil {
			compliance.RegisterHoldChecker(c)
			return true
		}
	}
	return false
}

// PluginSchema returns the JSON Schema for the named plugin's configuration.
// Returns nil if the plugin is not active or does not implement
// ConfigSchemaProvider. Safe to call from any goroutine. Takes a read lock.
func (a *Activator) PluginSchema(name string) map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	ap, ok := a.active[name]
	if !ok || ap.plugin == nil {
		return nil
	}
	if csp, ok := ap.plugin.(ConfigSchemaProvider); ok {
		return csp.ConfigSchema()
	}
	return nil
}

// Status returns a snapshot of current plugin state. Safe to call from any
// goroutine. Takes a read lock.
func (a *Activator) Status() PluginStatusReport {
	a.mu.RLock()
	defer a.mu.RUnlock()

	routes := make(map[string][]RouteDecl, len(a.routes))
	for _, pr := range a.routes {
		routes[pr.Name] = pr.Routes
	}

	plugins := make([]PluginStatus, 0, len(a.status))
	names := make([]string, 0, len(a.status))
	for name := range a.status {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := *a.status[name] // copy
		// The manifest is copied too, so a caller that changes the report
		// cannot change the next one.
		if s.Manifest != nil {
			m := *s.Manifest
			s.Manifest = &m
		}
		if ap, ok := a.active[name]; ok {
			s.Phase = ap.phase
			if !ap.startedAt.IsZero() {
				t := ap.startedAt
				s.StartedAt = &t
			}
			if !ap.stoppedAt.IsZero() {
				t := ap.stoppedAt
				s.StoppedAt = &t
			}
			if ap.lastError != nil {
				s.LastError = ap.lastError.Error()
			}
			s.Active = ap.phase == PhaseRunning
		}
		if s.Active {
			s.Routes = routeSummaries(routes[name])
		}
		plugins = append(plugins, s)
	}

	report := PluginStatusReport{
		Compiled: append([]string(nil), a.compiled...),
		Entitled: append([]string{}, a.entitled...),
		Plugins:  plugins,
	}
	if a.reqEnv != nil {
		report.Requested = append([]string(nil), a.reqEnv...)
	}
	return report
}

// routeSummaries lists decls without their handlers, sorted by pattern then
// method. A plugin that declares no routes gets nil, which the report omits.
func routeSummaries(decls []RouteDecl) []PluginRoute {
	if len(decls) == 0 {
		return nil
	}
	out := make([]PluginRoute, 0, len(decls))
	for _, d := range decls {
		out = append(out, PluginRoute{Method: d.Method, Pattern: d.Pattern, Group: d.Group})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// internal state mutators

func (a *Activator) recordPhase(name string, phase PluginPhase) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ap, ok := a.active[name]; ok {
		ap.phase = phase
		return
	}
	a.active[name] = &activatedPlugin{phase: phase}
}

func (a *Activator) recordRunning(name string, p core.Plugin) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap := a.active[name]
	if ap == nil {
		ap = &activatedPlugin{}
		a.active[name] = ap
	}
	ap.plugin = p
	ap.startedAt = time.Now().UTC()
	ap.phase = PhaseRunning
	ap.lastError = nil
}

func (a *Activator) recordFailure(name string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ap := a.active[name]
	if ap == nil {
		ap = &activatedPlugin{}
		a.active[name] = ap
	}
	ap.phase = PhaseFailed
	ap.lastError = err
}

// recordReason sets why a compiled plugin is not running.
func (a *Activator) recordReason(name, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.status[name]; ok {
		s.Reason = reason
	}
}

func (a *Activator) recordStopped(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ap, ok := a.active[name]; ok {
		ap.phase = PhaseStopped
		ap.stoppedAt = time.Now().UTC()
	}
}

// helpers

// parseRequested splits LYEVE_PLUGINS into a slice. Returns nil when the env
// is unset (caller distinguishes nil from empty slice). Whitespace and empty
// elements are dropped.
func parseRequested(env string) []string {
	env = strings.TrimSpace(env)
	if env == "" {
		return nil
	}
	parts := strings.Split(env, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		// Env was set but only contained whitespace/commas: treat as unset.
		return nil
	}
	sort.Strings(out)
	return out
}

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, x := range items {
		m[x] = true
	}
	return m
}

// unionStrings returns the sorted, de-duplicated union of a and b.
func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ReloadConfig asks every active plugin that implements core.Reloadable to
// rebuild its configuration-derived state, and reports how many did.
//
// Called after operator-set configuration changes. A plugin that reads its
// settings per request needs nothing here. This is for the ones that derived
// state in Start, such as a client built from an API key.
//
// One plugin's failure does not stop the others and does not stop the plugin
// that failed: it keeps the state it had. A bad value in one plugin's
// configuration must not take the process down.
func (a *Activator) ReloadConfig(ctx context.Context) (reloaded int, errs []error) {
	a.mu.RLock()
	type target struct {
		name string
		pl   core.Reloadable
	}
	targets := make([]target, 0, len(a.active))
	for name, ap := range a.active {
		if ap == nil || ap.plugin == nil {
			continue
		}
		if r, ok := ap.plugin.(core.Reloadable); ok {
			targets = append(targets, target{name: name, pl: r})
		}
	}
	a.mu.RUnlock()

	// Called with the lock released: a plugin's reload may take a lock of its
	// own, or call back into the host, and holding the activator's read lock
	// across that is how a deadlock gets built.
	for _, t := range targets {
		if err := t.pl.ReloadConfig(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.name, err))
			continue
		}
		reloaded++
	}
	return reloaded, errs
}
