package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/engine"
)

type parallelOnlyHost struct{ pe *engine.ParallelEngine }

func (h parallelOnlyHost) WorkerPool() *engine.WorkerPool               { return nil }
func (h parallelOnlyHost) GoroutineTracker() *engine.GoroutineTracker   { return nil }
func (h parallelOnlyHost) ParallelEngine() *engine.ParallelEngine       { return h.pe }
func (h parallelOnlyHost) AsyncHookExecutor() *engine.AsyncHookExecutor { return nil }
func (h parallelOnlyHost) DistLock(string) *engine.DistLock             { return nil }

func TestGoroutineTunables_ParallelLifecycle(t *testing.T) {
	pe := engine.NewParallelEngine(engine.ParallelConfig{MaxConcurrent: 4, Timeout: time.Second})
	tun := NewGoroutineTunables(parallelOnlyHost{pe: pe})

	view, ok := tun.ParallelConfig()
	require.True(t, ok)
	assert.False(t, view.Lifecycle, "the engine boots with the switch off")

	setter, ok := tun.(ParallelLifecycleSetter)
	require.True(t, ok, "the engine's tunables carry the lifecycle switch")
	view, err := setter.SetParallelLifecycle(true)
	require.NoError(t, err)
	assert.True(t, view.Lifecycle)
	assert.Equal(t, 4, view.MaxConcurrent, "the switch leaves the ceiling alone")

	view, err = tun.SetParallelConfig(GoroutineParallelSettings{MaxConcurrent: 8})
	require.NoError(t, err)
	assert.True(t, view.Lifecycle, "reconfiguring the ceiling keeps the switch")
	assert.Equal(t, 8, view.MaxConcurrent)
}
