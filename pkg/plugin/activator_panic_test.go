package plugin

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

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
