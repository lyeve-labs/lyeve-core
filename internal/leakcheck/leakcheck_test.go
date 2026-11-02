package leakcheck_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/leakcheck"
)

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
	before := leakcheck.Find(io.Discard)

	leakOne()

	// The goroutine may not have parked yet when leakOne returns.
	assert.Eventually(t, func() bool { return leakcheck.Find(io.Discard) > before }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, before+1, leakcheck.Find(&out))
	assert.Contains(t, out.String(), "leakOne")
}

func TestFind_IgnoresAGoroutineParkedOnAReachableChannel(t *testing.T) {
	before := leakcheck.Find(io.Discard)

	ch := make(chan struct{})
	done := make(chan struct{})
	go func() {
		<-ch
		close(done)
	}()

	assert.Equal(t, before, leakcheck.Find(io.Discard))
	close(ch)
	<-done
}
