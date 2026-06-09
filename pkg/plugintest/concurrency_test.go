package plugintest_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// The default pool is small so many suites can share one container per
// dialect, and it is invisible from inside a test: a burst wider than the pool
// queues on the pool rather than running, so a race needing more simultaneous
// sessions than the cap cannot happen and the test passes without ever
// reproducing it.
//
// These measure the cap directly, by counting how many callers hold a
// connection at the same moment. One dialect is enough: the cap is set by
// database/sql, not by the engine.
func TestHostPool_DefaultCapBoundsSimultaneousSessions(t *testing.T) {
	plugintest.SkipDialect(t, "postgres")
	assert.Equal(t, testdb.DefaultMaxConns, peakSessions(t, plugintest.Postgres(t), 20))
}

func TestHostPool_ConcurrentConstructorWidensTheCap(t *testing.T) {
	plugintest.SkipDialect(t, "postgres")
	assert.Equal(t, 16, peakSessions(t, plugintest.ConcurrentPostgres(t, 16), 20))
}

// peakSessions starts callers goroutines that each open a result set and hold
// it, which holds the connection, and reports how many held one at once.
//
// The query itself is trivial. What matters is the hold after it returns:
// database/sql only releases the connection on Close. Every caller holds until
// the peak has stopped rising, not for a fixed time, because opening the
// pool's connections can take longer than any fixed hold on a loaded runner. A
// 300 ms hold measured 15 of 16 that way, with the first caller gone before
// the last one arrived.
func peakSessions(t *testing.T, host core.Host, callers int) int {
	t.Helper()
	ctx := context.Background()

	var mu sync.Mutex
	inFlight, peak := 0, 0
	lastRise := time.Now()
	var errs []error

	start := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			rows, err := host.Querier(ctx).Query(ctx, `SELECT 1`)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
				lastRise = time.Now()
			}
			mu.Unlock()

			<-release

			mu.Lock()
			inFlight--
			mu.Unlock()
			rows.Close()
		}()
	}
	close(start)

	// The callers past the cap wait on the pool, so the count rises until the
	// pool has handed out every slot and then holds still. Two quiet seconds
	// end the hold, and the deadline ends it if a caller never arrives.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		quiet := peak > 0 && time.Since(lastRise) > 2*time.Second
		mu.Unlock()
		if quiet {
			break
		}
	}
	close(release)
	wg.Wait()

	require.Empty(t, errs, "a caller could not query")
	require.Positive(t, peak, "no caller reached the database")
	return peak
}
