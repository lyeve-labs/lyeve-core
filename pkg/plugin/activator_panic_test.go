package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/panicreport"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// panicHolder records the panics it is told of. With broken set, it panics
// instead.
type panicHolder struct {
	*fakePlugin
	broken bool
	got    []core.RecoveredPanic
}

func (p *panicHolder) ReportPanic(rp core.RecoveredPanic) {
	if p.broken {
		panic("reporter broke")
	}
	p.got = append(p.got, rp)
}

func TestReportPanic_ReachesEveryRunningHolder(t *testing.T) {
	first := &panicHolder{fakePlugin: newFakePlugin("alpha")}
	broken := &panicHolder{fakePlugin: newFakePlugin("beta"), broken: true}
	last := &panicHolder{fakePlugin: newFakePlugin("gamma")}
	stopped := &panicHolder{fakePlugin: newFakePlugin("delta")}
	stopped.startErr = errors.New("simulated start failure")
	a := startRolePlugins(t, first, broken, last, stopped, newFakePlugin("epsilon"))

	p := core.RecoveredPanic{TenantID: "acme", Method: "GET", Route: "/api/v1/items/{id}", Value: "boom"}
	assert.NotPanics(t, func() { a.ReportPanic(p) })

	assert.Equal(t, []core.RecoveredPanic{p}, first.got)
	assert.Equal(t, []core.RecoveredPanic{p}, last.got, "a holder that panics does not stop the rest")
	assert.Empty(t, stopped.got, "a plugin that is not running is not told")
}

// panickingPlugin panics in every call the activator makes on it, with the
// name of the call as the value.
type panickingPlugin struct{}

func (panickingPlugin) Name() string                           { return "boom" }
func (panickingPlugin) Start(context.Context, core.Host) error { panic("Start") }
func (panickingPlugin) Stop(context.Context) error             { panic("Stop") }
func (panickingPlugin) Routes() []RouteDecl                    { panic("Routes") }
func (panickingPlugin) FlowNodes() []core.FlowNode             { panic("FlowNodes") }
func (panickingPlugin) FlowTriggers() []core.FlowTrigger       { panic("FlowTriggers") }

// Each call the activator makes on a plugin recovers the plugin's panic and
// turns it into a failure for that plugin, so it has to report the panic too.
func TestPluginCalls_ReportTheRecoveredPanic(t *testing.T) {
	a := NewActivator(testHost{}, silentLogger)
	p := panickingPlugin{}
	for _, tc := range []struct {
		call string
		run  func()
	}{
		{"Start", func() { _ = safeStart(context.Background(), p, nil) }},
		{"Stop", func() { _ = safeStop(context.Background(), p) }},
		{"Routes", func() { a.safeRoutes("boom", p) }},
		{"FlowNodes", func() { _, _ = a.safeFlowNodes("boom", p) }},
		{"FlowTriggers", func() { _, _ = a.safeFlowTriggers("boom", p) }},
	} {
		t.Run(tc.call, func(t *testing.T) {
			var got []panicreport.Panic
			panicreport.Set(func(rp panicreport.Panic) { got = append(got, rp) })
			t.Cleanup(func() { panicreport.Set(nil) })

			tc.run()

			require.Len(t, got, 1, "the recovered panic was not reported")
			assert.Equal(t, "plugin/boom/"+tc.call, got[0].Goroutine)
			assert.Equal(t, tc.call, got[0].Value)
			assert.NotEmpty(t, got[0].Stack)
		})
	}
}
