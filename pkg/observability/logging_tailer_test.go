package observability

import (
	"fmt"
	"sync"
	"testing"
)

// Every goroutine that logs broadcasts, so the history must survive
// concurrent writers and readers. Run under -race.
func TestLogTailer_ConcurrentBroadcastAndRecent(t *testing.T) {
	tl := NewLogTailer()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				tl.Broadcast(LogEntry{Message: fmt.Sprintf("writer %d line %d", w, i)})
				if i%50 == 0 {
					_ = tl.Recent(TailFilter{}, 10)
				}
			}
		}()
	}
	wg.Wait()
	if got := len(tl.Recent(TailFilter{}, 0)); got != DefaultTailRingSize {
		t.Fatalf("history holds %d entries, want the ring's %d", got, DefaultTailRingSize)
	}
}
