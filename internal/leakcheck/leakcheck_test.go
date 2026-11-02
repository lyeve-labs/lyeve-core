package leakcheck_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/leakcheck"
)

// count returns how many leaked goroutines leakcheck.Find reports.
func count(t *testing.T, w io.Writer) int {
	t.Helper()
	n, err := leakcheck.Find(w)
	require.NoError(t, err)
	return n
}

// leakOne starts a goroutine that waits on a channel only it holds, so
// nothing can ever wake it.
//
//go:noinline
func leakOne() {
	ch := make(chan struct{})
	go func() { <-ch }()
}

func TestFind_ReportsAGoroutineParkedOnAnUnreachableChannel(t *testing.T) {
	var out strings.Builder
	before := count(t, io.Discard)

	leakOne()

	// The goroutine may not have parked yet when leakOne returns.
	assert.Eventually(t, func() bool { return count(t, io.Discard) > before }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, before+1, count(t, &out))
	assert.Contains(t, out.String(), "leakOne")
}

func TestFind_IgnoresAGoroutineParkedOnAReachableChannel(t *testing.T) {
	before := count(t, io.Discard)

	ch := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-ch
		close(done)
	}()

	assert.Equal(t, before, count(t, io.Discard))
	close(ch)
	<-done
}
