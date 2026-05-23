package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// fakePlugin is a core.Plugin implementation for tests. It records Start/Stop
// invocations and lets a test inject errors per phase.
type fakePlugin struct {
	name      string
	startErr  error
	stopErr   error
	mu        sync.Mutex
	started   bool
	stopped   bool
	startedAt time.Time
}

func newFakePlugin(name string) *fakePlugin { return &fakePlugin{name: name} }

func (p *fakePlugin) Name() string { return p.name }

func (p *fakePlugin) Start(ctx context.Context, host core.Host) error {
	if p.startErr != nil {
		return p.startErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	p.startedAt = time.Now()
	return nil
}

func (p *fakePlugin) Stop(ctx context.Context) error {
	if p.stopErr != nil {
		return p.stopErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	return nil
}

// silentLogger is a *slog.Logger that discards all output. Tests don't read
// log output. They assert behavior. Discarding keeps test output clean.
var silentLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// testHost satisfies core.Host with no-op implementations. The activator
// only invokes Host methods if a plugin's Start does, and the fake plugins
// in this file don't, so all returns are nil/zero.
type testHost struct{}

func (testHost) RawDB() *sql.DB       { return nil }
func (testHost) MigrationDB() *sql.DB { return nil }

func (testHost) Querier(ctx context.Context) core.Querier   { return nil }
func (testHost) QuerierRO(ctx context.Context) core.Querier { return nil }
func (testHost) Logger(ctx context.Context) *slog.Logger    { return silentLogger }
func (testHost) Config() core.Config                        { return nil }
func (testHost) Hooks() core.HookBus                        { return nil }
func (testHost) HookPublisher() core.HookPublisher          { return nil }
func (testHost) Version() string                            { return "test" }
func (testHost) Dialect() string                            { return "postgres" }
func (testHost) Schema() core.SchemaEngine                  { return nil }
func (testHost) Tracer(name string) trace.Tracer {
	return otel.GetTracerProvider().Tracer(name)
}

// CachedFetch caches nothing and runs the fetcher on every call. The fake
// plugins here never read through the host, but a stub that reports a miss
// without running the fetcher hands the caller an empty dest and hides every
// stale-read defect in the code under test, so this one still fetches.
func (testHost) CachedFetch(ctx context.Context, pluginName, tenantID, dialect, sql string, args []any, ttl time.Duration, dest any, fn core.CacheFetcher) (bool, error) {
	data, err := fn()
	if err != nil {
		return false, err
	}
	if dest == nil {
		return false, nil
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return false, fmt.Errorf("cached_fetch unmarshal: %w", err)
	}
	return false, nil
}

func (testHost) InvalidateCache(prefix string) int            { return 0 }
func (testHost) WorkerPool() *engine.WorkerPool               { return nil }
func (testHost) GoroutineTracker() *engine.GoroutineTracker   { return nil }
func (testHost) ParallelEngine() *engine.ParallelEngine       { return nil }
func (testHost) AsyncHookExecutor() *engine.AsyncHookExecutor { return nil }
func (testHost) DistLock(name string) *engine.DistLock        { return nil }
func (testHost) Capabilities() core.CapabilitySet {
	return core.CapabilitySet{Features: map[string]bool{}, Plan: "free", State: "free"}
}
func (testHost) HasFeature(ctx context.Context, feature string) bool { return false }

// The optional interfaces a fake host claims are reached by type assertion, so
// a signature that drifts from the real one fails the assertion silently and
// the code under test takes its uncached branch instead. Pin both.
var (
	_ core.Host               = testHost{}
	_ core.QueryCacheProvider = testHost{}
)

// withRegistered registers plugins for the duration of a test, then resets
// the registry. Returns the plugin instances by name so tests can inspect
// Start/Stop state.
func withRegistered(t *testing.T, names ...string) map[string]*fakePlugin {
	t.Helper()
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)

	plugins := make(map[string]*fakePlugin, len(names))
	for _, name := range names {
		// Capture name in factory closure.
		n := name
		p := newFakePlugin(n)
		plugins[n] = p
		RegisterPlugin(n, func() core.Plugin { return p })
	}
	return plugins
}

// Activation matrix

// TestDenyingPolicy_NoPluginsActivate verifies a policy that refuses every
// plugin: six plugins compiled in, none of them granted, none activate. Each
// shows the reason and the link the policy gave, because where to get a
// plugin is the policy's to say.
func TestDenyingPolicy_NoPluginsActivate(t *testing.T) {
	names := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	plugins := withRegistered(t, names...)
	refusal := licensing.PluginGrant{Reason: "the plan does not cover it", UpgradeURL: "https://example.test/upgrade"}
	deny := grantPolicy{}
	for _, n := range names {
		deny[n] = refusal
	}

	a := NewActivator(testHost{}, nil)
	a.Resolve(deny, "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: unexpected error: %v", err)
	}

	for name, p := range plugins {
		p.mu.Lock()
		started := p.started
		p.mu.Unlock()
		if started {
			t.Errorf("plugin %s started although the policy refused it", name)
		}
	}

	report := a.Status()
	if len(report.Entitled) != 0 {
		t.Errorf("expected nothing entitled; got %v", report.Entitled)
	}
	if len(report.Compiled) != 6 {
		t.Errorf("expected compiled=6; got %d", len(report.Compiled))
	}
	for _, s := range report.Plugins {
		if s.Active {
			t.Errorf("plugin %s reports Active=true under a denying policy", s.Name)
		}
		if s.Reason != refusal.Reason {
			t.Errorf("plugin %s reason = %q; want the policy's %q", s.Name, s.Reason, refusal.Reason)
		}
		if s.UpgradeURL != refusal.UpgradeURL {
			t.Errorf("plugin %s upgrade URL = %q; want the policy's %q", s.Name, s.UpgradeURL, refusal.UpgradeURL)
		}
	}
}

// TestOpenPolicy_EveryCompiledPluginActivates verifies a build that links no
// licensing implementation: licensing.Open grants every compiled plugin, so
// each one starts, and each one is ungated.
func TestOpenPolicy_EveryCompiledPluginActivates(t *testing.T) {
	names := []string{"alpha", "beta", "gamma"}
	plugins := withRegistered(t, names...)
	open, err := licensing.Open().NewManager(context.Background(), licensing.Env{Names: names})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	a := NewActivator(testHost{}, nil)
	a.Resolve(open, "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	report := a.Status()
	if !sliceEqual(report.Entitled, names) {
		t.Errorf("entitled=%v; want every compiled plugin %v", report.Entitled, names)
	}
	for _, name := range names {
		assertStarted(t, plugins, name, true)
		a.mu.RLock()
		isUngated := a.ungated[name]
		a.mu.RUnlock()
		if !isUngated {
			t.Errorf("plugin %s is not ungated under licensing.Open", name)
		}
	}
}

// TestPolicy_UngatedAndGrantedBothStart verifies a policy that marks some
// plugins ungated and grants another: the ungated plugins start beside the
// granted one, and a plugin neither names stays down.
func TestPolicy_UngatedAndGrantedBothStart(t *testing.T) {
	always := ungatedNames(t, 3)
	plugins := withRegistered(t, append(append([]string{}, always...), "alpha", "beta")...)

	a := NewActivator(testHost{}, nil)
	a.Resolve(ungated(always...).and(grants("alpha")), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	report := a.Status()
	want := append(append([]string{}, always...), "alpha")
	if !sameSet(report.Entitled, want) {
		t.Errorf("entitled=%v; want %v", report.Entitled, want)
	}
	for _, name := range always {
		assertStarted(t, plugins, name, true)
	}
	assertStarted(t, plugins, "alpha", true)
	assertStarted(t, plugins, "beta", false)
}

// TestResolve_EnvironmentDoesNotChangeEntitlement holds that entitlement is
// what the policy grants, in every environment, development included.
func TestResolve_EnvironmentDoesNotChangeEntitlement(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	always := ungatedNames(t, 2)
	plugins := withRegistered(t, append(append([]string{}, always...), "alpha", "beta")...)

	a := NewActivator(testHost{}, nil)
	a.Resolve(ungated(always...), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	report := a.Status()
	if !sameSet(report.Entitled, always) {
		t.Errorf("APP_ENV=development: entitled=%v; want only what the policy grants %v", report.Entitled, always)
	}
	// Plugins the policy does not grant stay inert in development too.
	assertStarted(t, plugins, "alpha", false)
	assertStarted(t, plugins, "beta", false)
}

// TestEntitledOnly_DefaultsToAll verifies env-unset behavior:
// license entitles two plugins -> exactly those two activate.
func TestEntitledOnly_DefaultsToAll(t *testing.T) {
	plugins := withRegistered(t, "alpha", "beta", "gamma")
	claims := grants("alpha", "gamma")

	a := NewActivator(testHost{}, nil)
	a.Resolve(claims, "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertStarted(t, plugins, "alpha", true)
	assertStarted(t, plugins, "gamma", true)
	assertStarted(t, plugins, "beta", false)

	report := a.Status()
	if !contains(report.Entitled, "alpha") || !contains(report.Entitled, "gamma") {
		t.Errorf("entitled missing alpha or gamma: %v", report.Entitled)
	}
	if report.Requested != nil {
		t.Errorf("expected nil Requested when env unset; got %v", report.Requested)
	}
}

// TestEnvSubset_RestrictsActivation verifies the env-override path:
// license entitles three, env requests one -> only that one activates.
func TestEnvSubset_RestrictsActivation(t *testing.T) {
	plugins := withRegistered(t, "alpha", "beta", "gamma")
	claims := grants("alpha", "beta", "gamma")

	a := NewActivator(testHost{}, nil)
	a.Resolve(claims, "alpha")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertStarted(t, plugins, "alpha", true)
	assertStarted(t, plugins, "beta", false)
	assertStarted(t, plugins, "gamma", false)

	report := a.Status()
	if len(report.Requested) != 1 || report.Requested[0] != "alpha" {
		t.Errorf("expected Requested=[alpha]; got %v", report.Requested)
	}
}

// TestEnvRequestsUnentitled_Refused verifies that env can only narrow, not widen:
// license entitles alpha, env requests eta -> eta does NOT activate.
// alpha also does NOT activate because env explicitly restricted to eta.
func TestEnvRequestsUnentitled_Refused(t *testing.T) {
	plugins := withRegistered(t, "alpha", "eta")
	claims := grants("alpha")

	a := NewActivator(testHost{}, nil)
	a.Resolve(claims, "eta")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertStarted(t, plugins, "alpha", false) // not requested
	assertStarted(t, plugins, "eta", false)   // not entitled

	report := a.Status()
	for _, s := range report.Plugins {
		if s.Name == "eta" && s.Reason == "" {
			t.Errorf("eta should have a non-empty reason: %+v", s)
		}
	}
}

// TestEnvRequestsUncompiled_Logged verifies the diagnostic path:
// env requests a name that isn't compiled in -> status report shows it,
// runtime doesn't crash.
func TestEnvRequestsUncompiled_Logged(t *testing.T) {
	withRegistered(t, "alpha")
	claims := grants("alpha", "ghost")

	a := NewActivator(testHost{}, nil)
	a.Resolve(claims, "alpha,ghost")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	report := a.Status()
	var ghost *PluginStatus
	for i := range report.Plugins {
		if report.Plugins[i].Name == "ghost" {
			ghost = &report.Plugins[i]
		}
	}
	if ghost == nil {
		t.Fatal("ghost should appear in status report even though it's not compiled")
	}
	if ghost.Compiled {
		t.Errorf("ghost should report Compiled=false")
	}
	if !strings.Contains(ghost.Reason, "not compiled") {
		t.Errorf("ghost reason should mention 'not compiled', got: %q", ghost.Reason)
	}
}

// TestPluginStartFailure_DoesNotAbortOthers verifies fault isolation:
// plugin A returns an error from Start -> plugins B, C still activate.
func TestPluginStartFailure_DoesNotAbortOthers(t *testing.T) {
	plugins := withRegistered(t, "alpha", "beta", "gamma")
	plugins["beta"].startErr = errors.New("simulated boom")
	claims := grants("alpha", "beta", "gamma")

	a := NewActivator(testHost{}, nil)
	a.Resolve(claims, "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertStarted(t, plugins, "alpha", true)
	assertStarted(t, plugins, "beta", false)
	assertStarted(t, plugins, "gamma", true)

	report := a.Status()
	for _, s := range report.Plugins {
		if s.Name == "beta" {
			if s.Phase != PhaseFailed {
				t.Errorf("beta Phase=%q; want failed", s.Phase)
			}
			if s.LastError == "" {
				t.Errorf("beta LastError should be set")
			}
		}
		if s.Name == "alpha" || s.Name == "gamma" {
			if s.Phase != PhaseRunning {
				t.Errorf("%s Phase=%q; want running", s.Name, s.Phase)
			}
		}
	}
}

// TestStopDrainsPlugins verifies the shutdown path:
// after Start, calling Stop invokes each running plugin's Stop().
func TestStopDrainsPlugins(t *testing.T) {
	plugins := withRegistered(t, "alpha", "gamma")
	claims := grants("alpha", "gamma")

	a := NewActivator(testHost{}, nil)
	a.Resolve(claims, "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	for _, name := range []string{"alpha", "gamma"} {
		p := plugins[name]
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if !stopped {
			t.Errorf("plugin %s Stop was not called", name)
		}
	}

	report := a.Status()
	for _, s := range report.Plugins {
		if s.Active {
			t.Errorf("plugin %s reports Active=true after Stop", s.Name)
		}
	}
}

// TestStartBeforeResolve_Errors guards against a programming error:
// calling Start before Resolve must return a clear error, not panic.
func TestStartBeforeResolve_Errors(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()
	a := NewActivator(testHost{}, nil)
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("Start before Resolve should error")
	}
}

// TestRegisterPlugin_PanicsOnDuplicate guards build-time misconfiguration.
func TestRegisterPlugin_PanicsOnDuplicate(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()
	RegisterPlugin("dup", func() core.Plugin { return newFakePlugin("dup") })
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	RegisterPlugin("dup", func() core.Plugin { return newFakePlugin("dup") })
}

// TestRegisterPlugin_PanicsOnEmpty guards against silent name typos.
func TestRegisterPlugin_PanicsOnEmpty(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty name")
		}
	}()
	RegisterPlugin("", func() core.Plugin { return newFakePlugin("") })
}

// TestParseRequested covers env-parsing edge cases.
func TestParseRequested(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,,", nil},
		{"alpha", []string{"alpha"}},
		{"alpha,gamma", []string{"alpha", "gamma"}},
		{"  alpha ,  gamma  ", []string{"alpha", "gamma"}},
		{"gamma,alpha", []string{"alpha", "gamma"}}, // sorted
		{"alpha,,gamma", []string{"alpha", "gamma"}},
	}
	for _, tt := range tests {
		got := parseRequested(tt.in)
		if !sliceEqual(got, tt.want) {
			t.Errorf("parseRequested(%q) = %v; want %v", tt.in, got, tt.want)
		}
	}
}

// helpers

func assertStarted(t *testing.T, plugins map[string]*fakePlugin, name string, want bool) {
	t.Helper()
	p, ok := plugins[name]
	if !ok {
		t.Fatalf("test bug: plugin %s not in map", name)
	}
	p.mu.Lock()
	got := p.started
	p.mu.Unlock()
	if got != want {
		t.Errorf("plugin %s started=%v; want %v", name, got, want)
	}
}

func contains(haystack []string, needle string) bool {
	for _, x := range haystack {
		if x == needle {
			return true
		}
	}
	return false
}

func sliceEqual(a, b []string) bool {
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

// sameSet reports whether a and b contain the same elements, ignoring order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// Dependency-aware activator tests

// TestDepResolver_AutoLoadsDep verifies that when a requested plugin
// depends on another entitled plugin, the dep is auto-loaded and
// started first.
func TestDepResolver_AutoLoadsDep(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	// beta depends on alpha.
	alpha := &depPlugin{name: "alpha"}
	beta := &depPlugin{
		name: "beta",
		deps: []PluginDependency{{Name: "alpha"}},
	}

	RegisterPlugin("beta", func() core.Plugin { return &depPluginNoVersion{beta} })
	RegisterPlugin("alpha", func() core.Plugin { return &depPluginNoVersion{alpha} })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("beta", "alpha"), "") // env unset -> both entitled
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Both should have started.
	if !alpha.started {
		t.Error("alpha (dep) was not started")
	}
	if !beta.started {
		t.Error("beta (depender) was not started")
	}

	report := a.Status()
	var alphaStatus, betaStatus *PluginStatus
	for i := range report.Plugins {
		switch report.Plugins[i].Name {
		case "alpha":
			alphaStatus = &report.Plugins[i]
		case "beta":
			betaStatus = &report.Plugins[i]
		}
	}
	if alphaStatus == nil || !alphaStatus.Active {
		t.Error("alpha should report active")
	}
	if betaStatus == nil || !betaStatus.Active {
		t.Error("beta should report active")
	}
	// alpha should have been auto-added (env-unset -> both entitled, both requested)
	// Since beta was in compiled list, it's the only "originally requested"
	// but with env-unset both are "requested" by the core defaults.
	// Verify the order: alpha should start before beta.
}

// TestDepResolver_DepStartsFirst verifies topological ordering: deps
// start before dependers.
func TestDepResolver_DepStartsFirst(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	var order []string

	// beta depends on alpha.
	alpha := &depPlugin{name: "alpha"}
	beta := &depPlugin{name: "beta", deps: []PluginDependency{{Name: "alpha"}}}

	// Override Start to record order.
	alphaF := &startOrderPlugin{depPlugin: alpha, rec: &order}
	betaF := &startOrderPlugin{depPlugin: beta, rec: &order}

	RegisterPlugin("beta", func() core.Plugin { return betaF })
	RegisterPlugin("alpha", func() core.Plugin { return alphaF })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("beta", "alpha"), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !alpha.started || !beta.started {
		t.Fatalf("both plugins should start: alpha=%v beta=%v", alpha.started, beta.started)
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 starts, got %d: %v", len(order), order)
	}
	if order[0] != "alpha" || order[1] != "beta" {
		t.Errorf("dep should start first: got %v, want [alpha, beta]", order)
	}
}

// depPlugin is a minimal core.Plugin for dependency-resolution tests.
type depPlugin struct {
	name    string
	deps    []PluginDependency
	version string

	started  bool
	stopped  bool
	startErr error
	stopErr  error
}

func (p *depPlugin) Name() string                     { return p.name }
func (p *depPlugin) Dependencies() []PluginDependency { return p.deps }
func (p *depPlugin) PluginVersion() string            { return p.version }
func (p *depPlugin) Start(context.Context, core.Host) error {
	p.started = true
	return p.startErr
}
func (p *depPlugin) Stop(context.Context) error {
	p.stopped = true
	return p.stopErr
}
func (p *depPlugin) Routes() []RouteDecl { return nil }
func (p *depPlugin) Store() any          { return nil }
func (p *depPlugin) Handler() any        { return nil }

// depPluginNoVersion wraps a depPlugin without reporting a version.
type depPluginNoVersion struct{ *depPlugin }

func (p *depPluginNoVersion) Version() string { return "" }

// depPluginWithVersion wraps a depPlugin reporting its version.
type depPluginWithVersion struct{ *depPlugin }

func (p *depPluginWithVersion) Version() string { return p.version }

// startOrderPlugin wraps a depPlugin to record startup order.
type startOrderPlugin struct {
	*depPlugin
	rec *[]string
}

func (p *startOrderPlugin) Start(ctx context.Context, h core.Host) error {
	*p.rec = append(*p.rec, p.Name())
	return p.depPlugin.Start(ctx, h)
}

// TestDepResolver_MissingDepFailsDepender verifies that a depender
// fails cleanly when its dependency is not in the registry.
func TestDepResolver_MissingDepFailsDepender(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	beta := &depPlugin{
		name: "beta",
		deps: []PluginDependency{{Name: "nonexistent"}},
	}
	RegisterPlugin("beta", func() core.Plugin { return &depPluginNoVersion{beta} })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("beta"), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// beta should NOT have started (its dep is missing).
	if beta.started {
		t.Error("beta should fail to start when dep is missing")
	}

	report := a.Status()
	for _, s := range report.Plugins {
		if s.Name == "beta" {
			if s.Active {
				t.Error("beta should not be active with missing dep")
			}
			if s.Phase != PhaseFailed {
				t.Errorf("beta phase=%q; want failed", s.Phase)
			}
			if s.LastError == "" {
				t.Error("beta should have an error message")
			}
		}
	}
}

// TestDepResolver_VersionMismatchFailsDepender verifies that a version
// constraint violation causes the depender to fail.
func TestDepResolver_VersionMismatchFailsDepender(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	beta := &depPlugin{
		name:    "beta",
		version: "1.0.0",
		deps:    []PluginDependency{{Name: "alpha", MinVersion: "2.0.0"}},
	}
	alpha := &depPlugin{name: "alpha", version: "1.0.0"}

	RegisterPlugin("beta", func() core.Plugin { return &depPluginWithVersion{beta} })
	RegisterPlugin("alpha", func() core.Plugin { return &depPluginWithVersion{alpha} })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("beta", "alpha"), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if beta.started {
		t.Error("beta should fail to start with version mismatch")
	}
	if !alpha.started {
		t.Error("alpha (dep) should still start even when depender fails")
	}

	report := a.Status()
	for _, s := range report.Plugins {
		if s.Name == "beta" {
			if s.Phase != PhaseFailed {
				t.Errorf("beta phase=%q; want failed", s.Phase)
			}
		}
	}
}

// TestDepResolver_CycleDetected verifies that circular dependencies
// prevent all cycle members from starting.
func TestDepResolver_CycleDetected(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	a := &depPlugin{name: "A", deps: []PluginDependency{{Name: "B"}}}
	b := &depPlugin{name: "B", deps: []PluginDependency{{Name: "A"}}}

	RegisterPlugin("A", func() core.Plugin { return &depPluginNoVersion{a} })
	RegisterPlugin("B", func() core.Plugin { return &depPluginNoVersion{b} })

	act := NewActivator(testHost{}, nil)
	act.Resolve(grants("A", "B"), "")
	if err := act.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if a.started {
		t.Error("A should not start (in a cycle)")
	}
	if b.started {
		t.Error("B should not start (in a cycle)")
	}

	report := act.Status()
	for _, s := range report.Plugins {
		if s.Name == "A" || s.Name == "B" {
			if s.Phase != PhaseFailed {
				t.Errorf("%s phase=%q; want failed", s.Name, s.Phase)
			}
		}
	}
}

// TestDepResolver_DiamondNoDuplicate verifies that a diamond dep graph
// starts deps only once and in correct order.
func TestDepResolver_DiamondNoDuplicate(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	var order []string

	base := &depPlugin{name: "base"}
	alpha := &depPlugin{name: "alpha", deps: []PluginDependency{{Name: "base"}}}
	beta := &depPlugin{
		name: "beta",
		deps: []PluginDependency{{Name: "alpha"}, {Name: "base"}},
	}

	RegisterPlugin("beta", func() core.Plugin { return &startOrderPlugin{beta, &order} })
	RegisterPlugin("alpha", func() core.Plugin { return &startOrderPlugin{alpha, &order} })
	RegisterPlugin("base", func() core.Plugin { return &startOrderPlugin{base, &order} })

	act := NewActivator(testHost{}, nil)
	act.Resolve(grants("beta", "alpha", "base"), "")
	if err := act.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(order) != 3 {
		t.Fatalf("expected 3 starts, got %d: %v", len(order), order)
	}
	if order[0] != "base" {
		t.Errorf("base must start first, got: %v", order)
	}
	// Check for duplicates.
	seen := make(map[string]bool)
	for _, name := range order {
		if seen[name] {
			t.Errorf("duplicate start for %s", name)
		}
		seen[name] = true
	}
}

// TestDepResolver_OnlyAutoLoadsEntitled verifies that an unentitled
// auto-dep is skipped and the depender starts without it.
func TestDepResolver_OnlyAutoLoadsEntitled(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	// The policy grants beta and nothing else, so gamma models a peer the
	// license does not cover.
	beta := &depPlugin{
		name: "beta",
		deps: []PluginDependency{{Name: "gamma"}},
	}
	gamma := &depPlugin{name: "gamma"}

	RegisterPlugin("beta", func() core.Plugin { return &depPluginNoVersion{beta} })
	RegisterPlugin("gamma", func() core.Plugin { return &depPluginNoVersion{gamma} })

	// Only beta is entitled. gamma is not.
	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("beta"), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// gamma isn't entitled so it shouldn't auto-load.
	if gamma.started {
		t.Error("unentitled gamma should not be auto-loaded")
	}
	if !beta.started {
		t.Error("beta should start even when its dep is not entitled, because dep resolution is best-effort")
	}
}

// TestDepResolver_NoDeps_WorksNormally verifies that plugins without
// Depender start normally.
func TestDepResolver_NoDeps_WorksNormally(t *testing.T) {
	plugins := withRegistered(t, "alpha", "gamma", "delta")
	claims := grants("alpha", "gamma", "delta")

	a := NewActivator(testHost{}, nil)
	a.Resolve(claims, "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertStarted(t, plugins, "alpha", true)
	assertStarted(t, plugins, "gamma", true)
	assertStarted(t, plugins, "delta", true)
}

// secret_source.go: Activator.SecretSource

func TestActivator_SecretSource_NilWhenNoProvider(t *testing.T) {
	// An activator with no running SecretSourceProvider declines with nil.
	if got := (&Activator{}).SecretSource(); got != nil {
		t.Errorf("expected nil from Activator with no provider, got %v", got)
	}
}
