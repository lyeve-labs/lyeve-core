package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// grantPolicy answers each plugin with the grant it lists, and refuses the
// rest with no reason of its own.
type grantPolicy map[string]licensing.PluginGrant

func (p grantPolicy) Plugin(name string) licensing.PluginGrant { return p[name] }

// grants starts exactly the named plugins, and marks none of them ungated.
func grants(names ...string) grantPolicy {
	p := grantPolicy{}
	for _, n := range names {
		p[n] = licensing.PluginGrant{Start: true}
	}
	return p
}

// ungated starts the named plugins whatever the license says, so a failure of
// one of them fails readiness.
func ungated(names ...string) grantPolicy {
	p := grantPolicy{}
	for _, n := range names {
		p[n] = licensing.PluginGrant{Start: true, Ungated: true}
	}
	return p
}

// and is p with every grant q lists added.
func (p grantPolicy) and(q grantPolicy) grantPolicy {
	out := grantPolicy{}
	for n, g := range p {
		out[n] = g
	}
	for n, g := range q {
		out[n] = g
	}
	return out
}

// ungatedNames returns n plugin names for fixtures a test policy marks
// ungated.
func ungatedNames(t *testing.T, n int) []string {
	t.Helper()
	names := []string{"anchor", "bedrock", "keel", "keystone", "pillar"}
	if n > len(names) {
		t.Fatalf("ungatedNames holds %d names; the test needs %d", len(names), n)
	}
	return append([]string(nil), names[:n]...)
}

func statusOf(t *testing.T, report PluginStatusReport, name string) PluginStatus {
	t.Helper()
	for _, s := range report.Plugins {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status row for %s", name)
	return PluginStatus{}
}

// A policy that names nothing beyond its answers has the report list the
// compiled plugins it lets start, and a refused plugin shows the reason and
// the URL the policy gave.
func TestResolve_APolicyWithoutAListReportsWhatItStarts(t *testing.T) {
	plugins := withRegistered(t, "alpha", "beta", "gamma")

	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(grantPolicy{
		"alpha": {Start: true},
		"gamma": {Start: true, Ungated: true},
		"beta":  {Reason: "the plan does not cover beta", UpgradeURL: "https://example.test/upgrade?plugin=beta"},
	}, "")
	require.NoError(t, a.Start(context.Background()))

	assertStarted(t, plugins, "alpha", true)
	assertStarted(t, plugins, "gamma", true)
	assertStarted(t, plugins, "beta", false)

	report := a.Status()
	assert.Equal(t, []string{"alpha", "gamma"}, report.Entitled)
	beta := statusOf(t, report, "beta")
	assert.False(t, beta.Entitled)
	assert.Equal(t, "the plan does not cover beta", beta.Reason)
	assert.Equal(t, "https://example.test/upgrade?plugin=beta", beta.UpgradeURL)
}

// A grant that gives no reason is shown with the activator's own, and a
// request through LYEVE_PLUGINS still says it was requested. Where to get a
// plugin is the licensing implementation's to say, so a grant that gives no
// URL shows none.
func TestResolve_AGrantWithNoReasonTakesTheActivatorsOwn(t *testing.T) {
	withRegistered(t, "alpha", "beta")

	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(grantPolicy{}, "alpha")

	report := a.Status()
	alpha := statusOf(t, report, "alpha")
	assert.Equal(t, "requested via LYEVE_PLUGINS but not granted by the capability set", alpha.Reason)
	assert.Empty(t, alpha.UpgradeURL)
	beta := statusOf(t, report, "beta")
	assert.Equal(t, "not granted by the capability set", beta.Reason)
	assert.Empty(t, beta.UpgradeURL)
}

// With no policy at all nothing starts, not even a plugin a policy would mark
// ungated, and the report still lists an empty set rather than null.
func TestResolve_ANilPolicyStartsNothing(t *testing.T) {
	names := ungatedNames(t, 1)
	plugins := withRegistered(t, names[0], "alpha")

	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(nil, "")
	require.NoError(t, a.Start(context.Background()))

	assertStarted(t, plugins, names[0], false)
	assertStarted(t, plugins, "alpha", false)
	b, err := json.Marshal(a.Status())
	require.NoError(t, err)
	assert.Contains(t, string(b), `"entitled":[]`)
}

// Readiness counts the failure of an ungated plugin and of no other, whatever
// the plugin is called.
func TestAllReady_OnlyAnUngatedFailureFailsReadiness(t *testing.T) {
	plugins := withRegistered(t, "alpha", "beta")
	plugins["alpha"].startErr = errors.New("alpha broke")
	plugins["beta"].startErr = errors.New("beta broke")

	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(grantPolicy{"alpha": {Start: true, Ungated: true}, "beta": {Start: true}}, "")
	require.NoError(t, a.Start(context.Background()))

	err := a.AllReady()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alpha broke")
	assert.NotContains(t, err.Error(), "beta")
}
