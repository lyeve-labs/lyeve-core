package api

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// signInCost runs serve on a goroutine locked to its thread and returns the
// CPU time that thread spent. What separates one sign-in answer from another
// is the password compare, which is CPU work. A busy runner delays the thread
// without making it work harder, so the figure stays put where wall-clock time
// stretches with every other process on the machine.
func signInCost(t testing.TB, serve func()) time.Duration {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before := threadCPUTime(t)
	serve()
	return threadCPUTime(t) - before
}

func threadCPUTime(t testing.TB) time.Duration {
	t.Helper()
	var ts unix.Timespec
	require.NoError(t, unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts))
	return time.Duration(ts.Nano())
}

// A password compare measured while other work holds every processor costs
// what it costs on a quiet runner, so the sign-in timing tests hold on a
// loaded one.
func TestSignInCost_ABusyRunnerDoesNotMoveIt(t *testing.T) {
	compare := func() { _ = auth.VerifyPassword("bcrypt", "", "not-the-password") }
	measure := func() time.Duration {
		samples := make([]time.Duration, 3)
		for i := range samples {
			samples[i] = signInCost(t, compare)
		}
		return median(samples)
	}

	quiet := measure()
	var stop atomic.Bool
	var spins atomic.Int64
	var wg sync.WaitGroup
	for range 2 * runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := int64(0)
			for !stop.Load() {
				n++
			}
			spins.Add(n)
		}()
	}
	busy := measure()
	stop.Store(true)
	wg.Wait()

	ratio := float64(busy) / float64(quiet)
	assert.True(t, ratio >= 0.5 && ratio <= 2.0, "busy runner: ratio %.2f, quiet %v, busy %v", ratio, quiet, busy)
}
