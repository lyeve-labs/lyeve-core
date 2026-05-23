package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// NewActivator constructs an Activator. host must be non-nil.
func NewActivator(host core.Host, logger *slog.Logger) *Activator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Activator{
		host:   host,
		logger: logger,
		active: map[string]*activatedPlugin{},
		status: map[string]*PluginStatus{},
		flows:  newFlowRegistry(),
	}
}

// RequireStateless limits the activator to plugins that run with no database.
// Call it before Resolve on an engine booted in stateless mode.
func (a *Activator) RequireStateless() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.statelessOnly = true
}

// needsDatabaseReason is the status reason of a plugin a stateless engine does
// not start.
const needsDatabaseReason = "needs a database; the engine is running in stateless mode"

// statelessCapable reports whether the compiled plugin name declares it runs
// with no database.
func statelessCapable(name string) bool {
	f, ok := pluginFactory(name)
	return ok && core.IsStatelessCapable(f())
}

// Policy says whether each compiled plugin may start. The runtime passes the
// licensing implementation's manager.
type Policy interface {
	Plugin(name string) licensing.PluginGrant
}

// EntitledLister is a Policy that names everything it entitles, plugins or
// not. The status report lists what it names. The report of a Policy without
// it lists the compiled plugins the Policy lets start.
type EntitledLister interface {
	EntitledNames() []string
}

// notGrantedReason is the status reason of a plugin its Policy refuses and
// gives no reason for.
const notGrantedReason = "not granted by the capability set"

// Resolve computes the activation set from the three gates: registered
// (compiled), entitled (what policy lets start), and requested (LYEVE_PLUGINS
// env). It populates the status map but does NOT start any plugin. Call Start
// for that.
//
// A nil policy lets nothing start. Compiled plugins the policy refuses appear
// inactive, with the reason and the upgrade URL it gives. A refusal that gives
// no reason shows the activator's own, and one that gives no URL shows none.
//
// reqEnv is the value of LYEVE_PLUGINS (or empty string when unset). Pass the
// raw env string. This method splits and trims it.
func (a *Activator) Resolve(policy Policy, reqEnv string) {
	compiled := RegisteredPlugins()

	grants := make(map[string]licensing.PluginGrant, len(compiled))
	ungated := make(map[string]bool, len(compiled))
	for _, name := range compiled {
		if policy == nil {
			continue
		}
		g := policy.Plugin(name)
		grants[name] = g
		if g.Ungated {
			ungated[name] = true
		}
	}

	// Entitlement comes from the policy only (there is no environment
	// override), so it is identical in every environment.
	entitled := []string{}
	if l, ok := policy.(EntitledLister); ok {
		entitled = unionStrings(l.EntitledNames(), nil)
	} else {
		for _, name := range compiled {
			if grants[name].Start {
				entitled = append(entitled, name)
			}
		}
	}

	requested := parseRequested(reqEnv)
	envSet := requested != nil

	a.mu.Lock()
	defer a.mu.Unlock()

	a.compiled = compiled
	a.entitled = entitled
	a.ungated = ungated
	a.reqEnv = requested
	a.resolved = true

	// Default: when env unset, requested = entitled (everything granted).
	effectiveRequested := requested
	if !envSet {
		effectiveRequested = entitled
	}

	compiledSet := toSet(compiled)
	entitledSet := toSet(entitled)
	requestedSet := toSet(effectiveRequested)

	// Build the status row for every compiled plugin.
	a.status = make(map[string]*PluginStatus, len(compiled))

	for _, name := range compiled {
		s := &PluginStatus{
			Name:      name,
			Compiled:  true,
			Entitled:  grants[name].Start,
			Requested: requestedSet[name],
			Phase:     PhaseRegistered,
		}
		// The version and the manifest come from one instance of the plugin,
		// read here and never started.
		if f, ok := pluginFactory(name); ok {
			p := f()
			if v, ok := p.(Versioned); ok {
				if ver := strings.TrimSpace(v.PluginVersion()); ver != "" {
					s.Version = ver
				}
			}
			if d, ok := p.(core.Describer); ok {
				s.Manifest = a.describe(name, d.Manifest())
			}
		}
		s.Reason, s.UpgradeURL = classifyInactive(s, grants[name])
		if a.statelessOnly && s.Reason == "" && !statelessCapable(name) {
			s.Reason = needsDatabaseReason
		}
		a.status[name] = s
	}

	// Surface any env-requested names that aren't compiled. They never run.
	// Log them so operators know why their override didn't take effect.
	if envSet {
		for _, name := range requested {
			if compiledSet[name] {
				continue
			}
			a.logger.Warn("LYEVE_PLUGINS requested a plugin not compiled into this binary",
				"plugin", name)
			// Add a synthetic status row so the introspection endpoint shows it.
			a.status[name] = &PluginStatus{
				Name:      name,
				Compiled:  false,
				Entitled:  entitledSet[name],
				Requested: true,
				Phase:     PhaseRegistered,
				Reason:    "plugin not compiled into this build",
			}
		}
	}
}

// describe is what a plugin says about itself, normalized for its status row.
// A category or maturity outside the closed set is dropped and logged, because
// the plugin's author can fix it and an operator cannot. A manifest that names
// no label takes the plugin's name, so a row that carries a manifest always
// has a label to show.
func (a *Activator) describe(name string, raw core.PluginManifest) *core.PluginManifest {
	m := core.NormalizeManifest(raw)
	if (raw.Category != "" && m.Category == "") || (raw.Maturity != "" && m.Maturity == "") {
		a.logger.Warn("plugin manifest names a category or maturity the engine does not know, so the plugin status leaves it out",
			"plugin", name, "category", raw.Category, "maturity", raw.Maturity)
	}
	if m.Label == "" {
		m.Label = name
	}
	return &m
}

// classifyInactive returns (reason, upgradeURL) when the status row represents
// a plugin that won't activate. Returns empty strings for plugins that are
// fully entitled and requested. A refused plugin carries the reason and the
// URL its grant gives. Where to get a plugin is the licensing
// implementation's to say, so a grant that names no URL shows none.
func classifyInactive(s *PluginStatus, g licensing.PluginGrant) (string, string) {
	refusal, upgrade := g.Reason, g.UpgradeURL
	if refusal == "" {
		refusal = notGrantedReason
	}
	switch {
	case !s.Entitled && !s.Requested:
		return refusal, upgrade
	case !s.Entitled && s.Requested:
		return "requested via LYEVE_PLUGINS but " + refusal, upgrade
	case s.Entitled && !s.Requested:
		return "granted but not requested via LYEVE_PLUGINS", ""
	default:
		// entitled && requested -> will activate. No inactive reason
		return "", ""
	}
}

// Start activates the plugins that pass all three gates (compiled, entitled,
// requested). Plugins grouped by dependency level start in parallel within
// each level. Successive levels wait for all plugins in the previous level.
//
// Lazy plugins (implementing LazyPlugin.IsLazy()) are NOT started at boot.
// Instead their LazyRoutes are collected and their phase set to PhaseLazy.
// Call OnDemandStart to activate them on first use.
//
// Before starting, dependency resolution computes the transitive closure and
// topologically sorts so dependencies start first. A plugin implementing
// core.Depender declares its peer requirements. A requirement that is not
// compiled into the binary fails the depender: nothing at runtime can create
// its tables. A requirement the capability set does not grant is skipped and
// the depender still starts, so a plugin naming an ungranted peer does not
// fail because of it.
//
// Resolve must be called first. If a plugin's Start returns an error, the
// failure is recorded in status and the activator continues: one bad plugin
// does not abort the whole startup.
func (a *Activator) Start(ctx context.Context) error {
	a.mu.Lock()
	if !a.resolved {
		a.mu.Unlock()
		return fmt.Errorf("core.Activator: Start called before Resolve")
	}

	type entry struct {
		name   string
		status *PluginStatus
	}
	var queue []entry
	for _, name := range a.compiled {
		s := a.status[name]
		if s == nil || !s.Entitled || !s.Requested {
			continue
		}
		queue = append(queue, entry{name: name, status: s})
	}
	// Snapshot entitlement set to auto-load deps that are entitled
	// but not originally requested.
	entitledSet := make(map[string]bool, len(a.entitled))
	for _, name := range a.entitled {
		entitledSet[name] = true
	}
	a.mu.Unlock()

	// Dependency resolution
	requestedNames := make([]string, len(queue))
	for i, e := range queue {
		requestedNames[i] = e.name
	}
	resolution := ResolvePluginDeps(requestedNames)

	// Log all resolution errors.
	for _, e := range resolution.Missing {
		a.logger.Error("plugin dependency missing",
			"plugin", e.Plugin,
			"dep", e.Dep,
			"message", e.Message,
		)
	}
	for _, e := range resolution.Version {
		a.logger.Error("plugin dependency version mismatch",
			"plugin", e.Plugin,
			"dep", e.Dep,
			"message", e.Message,
		)
	}
	if len(resolution.Cycle) > 0 {
		a.logger.Error("plugin dependency cycle detected",
			"plugins", resolution.Cycle,
		)
	}

	// Build failure sets so dependent plugins can be skipped. Missing deps are
	// kept by name: the operator reads this through the plugin status endpoint,
	// where "one or more required plugins" is not something they can act on.
	missingSet := make(map[string][]string)
	for _, e := range resolution.Missing {
		missingSet[e.Plugin] = append(missingSet[e.Plugin], e.Dep)
	}
	versionSet := make(map[string]bool)
	for _, e := range resolution.Version {
		versionSet[e.Plugin] = true
	}
	cycleSet := make(map[string]bool, len(resolution.Cycle))
	for _, name := range resolution.Cycle {
		cycleSet[name] = true
	}

	// Ensure auto-added deps have status records. Mark them as requested.
	for _, name := range resolution.AutoAdded {
		a.mu.Lock()
		if s, ok := a.status[name]; ok && s.Entitled {
			s.Requested = true
			s.Reason = "" // clear any previous "not requested" reason
		}
		a.mu.Unlock()
	}

	// Determine load order
	// Use topological order if resolution succeeded without cycles.
	var names []string
	if len(resolution.Order) > 0 {
		names = resolution.Order
	} else {
		names = requestedNames
	}

	// Map of original-queue entries for fast lookup.
	queueMap := make(map[string]entry, len(queue))
	for _, e := range queue {
		queueMap[e.name] = e
	}

	// Separate lazy plugins
	// Lazy plugins: skip Start(), record as PhaseLazy, collect LazyRoutes.
	lazySet := make(map[string]bool)

	// Pre-scan: instantiate and check IsLazy() for each plugin.
	//
	// On a stateless engine a plugin that needs a database is dropped here,
	// before anything can start it, lazily or as another plugin's dependency.
	pluginInstances := make(map[string]core.Plugin, len(names))
	needsDatabase := make(map[string]bool)
	for _, name := range names {
		factory, ok := pluginFactory(name)
		if !ok {
			continue
		}
		p := factory()
		if a.statelessOnly && !core.IsStatelessCapable(p) {
			needsDatabase[name] = true
			a.recordReason(name, needsDatabaseReason)
			a.logger.Info("plugin not started: it needs a database", "plugin", name)
			continue
		}
		if lp, ok := p.(LazyPlugin); ok && lp.IsLazy() {
			lazySet[name] = true
			pluginInstances[name] = p
			a.recordPhase(name, PhaseLazy)
			a.logger.Info("plugin deferred (lazy)", "plugin", name)

			// Collect lazy routes immediately so they're mounted before Start.
			lazyRoutes := lp.LazyRoutes()
			if len(lazyRoutes) > 0 {
				if err := a.collectRoutes(name, lazyRoutes); err != nil {
					a.logger.Error("lazy plugin route collection failed", "plugin", name, "err", err)
					a.recordFailure(name, fmt.Errorf("lazy route conflict: %w", err))
				}
			}
		} else {
			pluginInstances[name] = p
		}
	}

	// Helper: start a single non-lazy plugin
	startOne := func(name string) {
		// Skip plugins that failed dep resolution.
		if missing := missingSet[name]; len(missing) > 0 {
			a.recordFailure(name, fmt.Errorf("dependency resolution failed: %s not registered", strings.Join(missing, ", ")))
			return
		}
		if versionSet[name] {
			a.recordFailure(name, fmt.Errorf("dependency resolution failed: version constraint not satisfied"))
			return
		}
		if cycleSet[name] {
			a.recordFailure(name, fmt.Errorf("dependency resolution failed: circular dependency detected"))
			return
		}
		// Lazy plugins already handled.
		if lazySet[name] || needsDatabase[name] {
			return
		}
		// A dependency the capability set does not grant is skipped, not
		// failed, and the depender still starts. Recording a failure here would
		// put an install into PhaseFailed whenever a granted plugin names an
		// ungranted peer. Leave the status row as Resolve classified it.
		if _, wasRequested := queueMap[name]; !wasRequested && !entitledSet[name] {
			a.logger.Warn("plugin dependency not granted by the capability set",
				"plugin", name,
				"required_by", resolution.Requires[name],
			)
			return
		}

		p := pluginInstances[name]
		if p == nil {
			a.logger.Error("plugin instance missing at activate time", "plugin", name)
			a.recordFailure(name, fmt.Errorf("instance missing"))
			return
		}
		a.recordPhase(name, PhaseStarting)
		a.logger.Info("starting plugin", "plugin", name)
		caps := CapPolicy(name)
		scoped := core.NewScopedHost(a.host, name, caps)
		if err := safeStart(ctx, p, scoped); err != nil {
			a.logger.Error("plugin start failed", "plugin", name, "err", err)
			a.recordFailure(name, err)
			return
		}
		a.recordRunning(name, p)
		a.logger.Info("plugin started", "plugin", name)

		// Collect route declarations from plugins that implement RoutesPlugin.
		if rp, ok := p.(RoutesPlugin); ok {
			routes := rp.Routes()
			if len(routes) > 0 {
				if err := a.collectRoutes(name, routes); err != nil {
					a.logger.Error("plugin route collection failed", "plugin", name, "err", err)
					if stopErr := safeStop(ctx, p); stopErr != nil {
						a.logger.Error("plugin stop after route conflict failed", "plugin", name, "err", stopErr)
					}
					a.recordFailure(name, fmt.Errorf("route conflict: %w", err))
				}
			}
		}
	}

	// Parallel start by dependency level
	if len(resolution.Levels) > 0 {
		// Parallel within each level, sequential between levels.
		for lvlIdx, level := range resolution.Levels {
			a.logger.Info("starting plugin level",
				"level", lvlIdx, "plugins", level, "count", len(level))
			g, _ := errgroup.WithContext(ctx)
			for _, name := range level {
				// capture
				g.Go(func() error {
					startOne(name)
					return nil // never fail the group: fault isolation
				})
			}
			_ = g.Wait() // errors are logged, not propagated
			// Check the parent context for early abort on shutdown
			// signal: don't use the errgroup-derived context which
			// cancels on Wait.
			if ctx.Err() != nil {
				a.logger.Warn("plugin start aborted by context", "err", ctx.Err(), "level", lvlIdx)
				return nil
			}
		}
	} else {
		// No levels: sequential start.
		for _, name := range names {
			startOne(name)
		}
	}

	a.logStartSummary()
	// Every level has started, so the registry can be built from what is
	// running. Before this point a plugin that reads it would see a subset.
	nodes, triggers := a.RebuildFlowRegistry()
	a.logger.Info("flow registry built", "nodes", nodes, "triggers", triggers)
	return nil
}

// logStartSummary emits one line naming every plugin that failed to start.
//
// A failure is already recorded in status and readable from Status(), and one
// of a plugin its grant does not mark ungated deliberately does not fail
// readiness. Neither is
// visible to an operator watching a boot: the per-plugin error is one line
// among many, and reaching Status() needs an authenticated admin request. So
// one summary line names every failure.
//
// Logged at Error only when a plugin readiness requires failed, since that is
// the case readiness also rejects. Any other failure is a warning.
func (a *Activator) logStartSummary() {
	a.mu.RLock()
	var failed, failedUngated []string
	for name, ap := range a.active {
		if ap.phase != PhaseFailed {
			continue
		}
		failed = append(failed, name)
		if a.ungated[name] {
			failedUngated = append(failedUngated, name)
		}
	}
	running := 0
	for _, ap := range a.active {
		if ap.phase == PhaseRunning {
			running++
		}
	}
	a.mu.RUnlock()

	if len(failed) == 0 {
		a.logger.Info("all plugins started", "running", running)
		return
	}
	sort.Strings(failed)
	sort.Strings(failedUngated)
	args := []any{
		"failed", len(failed),
		"running", running,
		"plugins", failed,
	}
	if len(failedUngated) > 0 {
		a.logger.Error("plugins failed to start, including required plugins",
			append(args, "ungated", failedUngated)...)
		return
	}
	a.logger.Warn("plugins failed to start, their features are unavailable", args...)
}

// OnDemandStart activates a lazy plugin by name. Called when a request
// hits a lazy plugin's route. Safe to call concurrently. A running plugin
// is a no-op (Start is not called twice).
func (a *Activator) OnDemandStart(ctx context.Context, name string) error {
	// phase is copied under the lock. recordPhase writes it while holding the
	// same mutex, so reading it off the pointer afterwards races with any
	// concurrent transition of this plugin, and two requests arriving on a lazy
	// plugin's route at once is the ordinary case here rather than a corner.
	a.mu.RLock()
	ap, ok := a.active[name]
	var phase PluginPhase
	if ok {
		phase = ap.phase
	}
	a.mu.RUnlock()

	if !ok || phase != PhaseLazy {
		if ok && phase == PhaseRunning {
			return nil // already started
		}
		return fmt.Errorf("plugin %q is not lazy or not found", name)
	}

	factory, ok := pluginFactory(name)
	if !ok {
		return fmt.Errorf("plugin %q factory missing", name)
	}

	p := factory()
	a.recordPhase(name, PhaseStarting)
	a.logger.Info("on-demand starting plugin", "plugin", name)
	caps := CapPolicy(name)
	scoped := core.NewScopedHost(a.host, name, caps)
	if err := safeStart(ctx, p, scoped); err != nil {
		a.logger.Error("on-demand plugin start failed", "plugin", name, "err", err)
		a.recordFailure(name, err)
		return err
	}
	a.recordRunning(name, p)
	a.logger.Info("on-demand plugin started", "plugin", name)

	// Collect routes: lazy plugins already had LazyRoutes mounted,
	// but Routes() may differ (e.g., dynamic routes after activation).
	if rp, ok := p.(RoutesPlugin); ok {
		routes := rp.Routes()
		if len(routes) > 0 {
			if err := a.collectRoutes(name, routes); err != nil {
				a.logger.Error("on-demand plugin route collection failed", "plugin", name, "err", err)
				if stopErr := p.Stop(ctx); stopErr != nil {
					a.logger.Error("on-demand plugin stop after route conflict failed", "plugin", name, "err", stopErr)
				}
				a.recordFailure(name, fmt.Errorf("route conflict: %w", err))
				return err
			}
		}
	}
	a.RebuildFlowRegistry()
	return nil
}

// AllReady returns nil when every running plugin that implements
// ReadinessReporter reports Ready. Lazy plugins not yet started, and
// plugins that don't implement the interface, are considered ready.
// Returns a combined error when any running plugin is not ready.
//
// A failed plugin whose grant marks it ungated is never ready. Those are the
// plugins every install runs whatever its license, and reporting only on
// PhaseRunning would let /readyz answer 200 for an install whose required
// plugins had failed to start at all, and an orchestrator would route traffic
// to it.
//
// Any other failed plugin is recorded in Status but does not fail
// readiness. It is one feature that is down, not a broken install, and the
// operator cannot fix it by restarting: a plugin whose declared peer
// is absent from the binary fails on every boot. Failing readiness on it makes
// /readyz answer 503 forever, which under an orchestrator means the process
// never receives traffic at all. Start documents the same split: one bad
// plugin does not abort the whole startup.
func (a *Activator) AllReady() error {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var errs []string
	for name, ap := range a.active {
		if ap.phase == PhaseFailed {
			if a.ungated[name] {
				errs = append(errs, fmt.Sprintf("%s: start failed: %v", name, ap.lastError))
			}
			continue
		}
		if ap.plugin == nil || ap.phase != PhaseRunning {
			continue
		}
		rr, ok := ap.plugin.(ReadinessReporter)
		if !ok {
			continue
		}
		if err := rr.Ready(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("plugins not ready: %s", strings.Join(errs, "; "))
	}
	return nil
}

// PluginReadinessProbe returns a function suitable for use as a ProbeCheck
// in the health probe registry. It calls AllReady and returns an error when
// any running plugin reports not-ready.
func (a *Activator) PluginReadinessProbe() func(context.Context) error {
	return func(ctx context.Context) error {
		return a.AllReady()
	}
}

// collectRoutes validates route declarations for name and merges them with
// any existing routes for the same plugin. Returns an error if any route
// conflicts with a previously-collected route from another plugin.
// Locks a.mu itself.
func (a *Activator) collectRoutes(name string, routes []RouteDecl) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	seen := make(map[string]bool, len(routes))
	for i, rd := range routes {
		key := rd.Method + " " + rd.Pattern
		if seen[key] {
			return fmt.Errorf("plugin %q declares duplicate route %q (index %d and earlier)", name, key, i)
		}
		seen[key] = true

		// Check for conflicts with already-collected plugins (excluding self).
		for _, existing := range a.routes {
			if existing.Name == name {
				continue // self-routes are merged, not conflicting
			}
			for _, erd := range existing.Routes {
				ek := erd.Method + " " + erd.Pattern
				if ek == key {
					return fmt.Errorf("route %q claimed by %q conflicts with existing route from %q", key, name, existing.Name)
				}
			}
		}
	}

	// Merge: if this plugin already has collected routes, append new ones.
	// This handles lazy plugins where LazyRoutes are mounted at boot and
	// Routes() are added after OnDemandStart.
	for i, existing := range a.routes {
		if existing.Name == name {
			a.routes[i].Routes = append(a.routes[i].Routes, routes...)
			// Notify listener of changed routes.
			if a.onRoutesChanged != nil {
				a.onRoutesChanged(a.routes)
			}
			return nil
		}
	}
	a.routes = append(a.routes, PluginRoutes{Name: name, Routes: routes})
	return nil
}
