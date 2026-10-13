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
