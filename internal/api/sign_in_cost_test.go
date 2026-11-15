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
// loaded one. The quiet and busy samples alternate, and each round swaps
// which goes first, so a change in the host's own load during the test
// reaches both sides alike. A thread's CPU time still depends on the core it
// lands on and on whether a hyperthread sibling is busy, so one sample can
// read twice another. The medians over many rounds compare the two
// conditions rather than two placements.
func TestSignInCost_ABusyRunnerDoesNotMoveIt(t *testing.T) {
	compare := func() { _ = auth.VerifyPassword("bcrypt", "", "not-the-password") }
	const rounds = 15
	quiet := make([]time.Duration, 0, rounds)
	busy := make([]time.Duration, 0, rounds)
	measureBusy := func() {
		stop := occupyEveryProcessor()
		busy = append(busy, signInCost(t, compare))
		stop()
	}
	for i := range rounds {
		if i%2 == 0 {
			quiet = append(quiet, signInCost(t, compare))
			measureBusy()
		} else {
			measureBusy()
			quiet = append(quiet, signInCost(t, compare))
		}
	}

	q, b := median(quiet), median(busy)
	ratio := float64(b) / float64(q)
	assert.True(t, ratio >= 0.5 && ratio <= 2.0, "busy runner: ratio %.2f, quiet %v, busy %v", ratio, q, b)
}

// occupyEveryProcessor starts two spinning goroutines per processor and
// returns once every one of them has started. The function it returns stops
// them and waits until they have.
func occupyEveryProcessor() (stop func()) {
	var halt atomic.Bool
	var spins atomic.Int64
	var started, done sync.WaitGroup
	for range 2 * runtime.GOMAXPROCS(0) {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			started.Done()
			n := int64(0)
			for !halt.Load() {
				n++
			}
			spins.Add(n)
		}()
	}
	started.Wait()
	return func() {
		halt.Store(true)
		done.Wait()
	}
}
