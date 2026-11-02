package testdb

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A test takes what the background prepared, and no database goes to two
// tests.
func TestReadyQueue_TakesWhatTheBackgroundPrepared(t *testing.T) {
	var made atomic.Int64
	q := &readyQueue{}
	q.start(func() readyDB { return readyDB{name: fmt.Sprintf("ready_%d", made.Add(1))} })
	require.Eventually(t, func() bool { return len(q.ch) == readyAhead }, 5*time.Second, time.Millisecond)

	seen := map[string]bool{}
	for range 3 * readyAhead {
		r := q.take(func() readyDB {
			t.Error("a test prepared its own database while one was ready")
			return readyDB{name: "own"}
		})
		require.NoError(t, r.err)
		assert.False(t, seen[r.name], "%s went to two tests", r.name)
		seen[r.name] = true
		require.Eventually(t, func() bool { return len(q.ch) == readyAhead }, 5*time.Second, time.Millisecond)
	}
}

// When the background is still busy, a test prepares its own instead of
// waiting, so tests running in parallel never queue behind it.
func TestReadyQueue_PreparesItsOwnWhenNoneIsReady(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	q := &readyQueue{}
	q.start(func() readyDB {
		<-hold
		return readyDB{name: "background"}
	})

	r := q.take(func() readyDB { return readyDB{name: "own"} })
	assert.Equal(t, "own", r.name)
}

// A preparation that fails reaches the test that takes it, with the name of a
// database it created so the test can drop it.
func TestReadyQueue_HandsOnAFailedPreparation(t *testing.T) {
	q := &readyQueue{}
	q.start(func() readyDB { return readyDB{name: "half", err: fmt.Errorf("migrate half: refused")} })
	require.Eventually(t, func() bool { return len(q.ch) > 0 }, 5*time.Second, time.Millisecond)

	r := q.take(func() readyDB { return readyDB{name: "own"} })
	assert.Equal(t, "half", r.name)
	assert.EqualError(t, r.err, "migrate half: refused")
}

// StopPreparation returns only once the preparation already running has
// finished, and no preparation starts after it, so a TestMain that lists the
// goroutines still running sees none of a preparation's connections.
func TestStopPreparation_WaitsForThePreparationInFlight(t *testing.T) {
	running := make(chan struct{})
	finish := make(chan struct{})
	var started atomic.Int64
	q := &readyQueue{}
	q.start(func() readyDB {
		if started.Add(1) == 1 {
			close(running)
		}
		<-finish
		return readyDB{name: "background"}
	})
	<-running

	stopped := make(chan struct{})
	go func() {
		StopPreparation()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("StopPreparation returned while a preparation was running")
	case <-time.After(50 * time.Millisecond):
	}

	close(finish)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopPreparation did not return once the preparation finished")
	}
	defer preparing.Unlock()
	n := started.Load()
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, n, started.Load(), "a preparation started after StopPreparation returned")
}
