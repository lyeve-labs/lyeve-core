package plugin

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// maskingReporter holds the response masking role and reports the control it
// supplies, recording what the engine told it.
type maskingReporter struct {
	*fakePlugin
	mu   sync.Mutex
	told []core.ControlEnv
}

func (p *maskingReporter) PIIMiddleware() func(http.Handler) http.Handler { return nil }

func (p *maskingReporter) SecurityControls(env core.ControlEnv) []core.SecurityControl {
	p.mu.Lock()
	p.told = append(p.told, env)
	p.mu.Unlock()
	status := core.ControlSkip
	if env.Running && env.Wired["core.PIIMiddlewareProvider"] {
		status = core.ControlPass
	}
	return []core.SecurityControl{{Control: "masking/" + p.name, Status: status, Detail: env.InstanceRegion}}
}

func (p *maskingReporter) lastTold(t *testing.T) core.ControlEnv {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.NotEmpty(t, p.told, "%s was never asked", p.name)
	return p.told[len(p.told)-1]
}

// Every compiled reporter answers, in name order, whether it runs or not. The
// one that runs is told so and which of its roles are wired, and the one that
// does not reads SKIP instead of vanishing from the report.
func TestReportControls_AsksEveryCompiledReporterInNameOrder(t *testing.T) {
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	running := &maskingReporter{fakePlugin: newFakePlugin("masker")}
	idle := &maskingReporter{fakePlugin: newFakePlugin("idle-masker")}
	for _, p := range []core.Plugin{running, idle, newFakePlugin("plain")} {
		RegisterPlugin(p.Name(), func() core.Plugin { return p })
	}
	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(grants("masker", "plain"), "")
	require.NoError(t, a.Start(context.Background()))

	rows := a.ReportControls(core.ControlEnv{InstanceRegion: "eu-west", MFALockout: true})
	assert.Equal(t, []core.SecurityControl{
		{Control: "masking/idle-masker", Status: core.ControlSkip, Detail: "eu-west"},
		{Control: "masking/masker", Status: core.ControlPass, Detail: "eu-west"},
	}, rows)

	told := running.lastTold(t)
	assert.True(t, told.Running)
	assert.True(t, told.MFALockout, "the install's facts reach the reporter")
	assert.Equal(t, map[string]bool{
		"core.PIIMiddlewareProvider":   true,
		"core.SecurityControlReporter": true,
	}, told.Wired)

	told = idle.lastTold(t)
	assert.False(t, told.Running)
	assert.Empty(t, told.Wired, "nothing of a plugin that does not run is wired")
}

// A single role two running plugins implement is wired from neither, and each
// reporter is told so.
func TestReportControls_ARoleTwoPluginsClaimIsWiredForNeither(t *testing.T) {
	first := &maskingReporter{fakePlugin: newFakePlugin("masker-a")}
	second := &maskingReporter{fakePlugin: newFakePlugin("masker-b")}
	a := startRolePlugins(t, first, second)

	rows := a.ReportControls(core.ControlEnv{})
	require.Len(t, rows, 2)
	for _, r := range rows {
		assert.Equal(t, core.ControlSkip, r.Status, r.Control)
	}
	assert.False(t, first.lastTold(t).Wired["core.PIIMiddlewareProvider"])
	assert.True(t, first.lastTold(t).Wired["core.SecurityControlReporter"])
}
