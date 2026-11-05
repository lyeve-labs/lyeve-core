package logstream_test

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/logstream"
)

// Every log call in the process pushes into the ring, so a matcher that runs
// while the ring is locked stalls every request for as long as it takes. A
// filtered export is a tenant admin's request, so how long that is depends on
// what the caller asks for.
func TestBuffer_SnapshotMatcherDoesNotBlockPush(t *testing.T) {
	b := logstream.NewBuffer(16)
	b.Push(logstream.Entry{Message: "held"})

	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		b.Snapshot(func(logstream.Entry) bool {
			close(entered)
			<-release
			return true
		}, 0)
	}()
	<-entered

	pushed := make(chan struct{})
	go func() {
		b.Push(logstream.Entry{Message: "next"})
		close(pushed)
	}()
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Error("a push waited for a snapshot's matcher")
	}
	close(release)
	<-done
	<-pushed
}

// BenchmarkBuffer_PushDuringFilteredExport measures what a log call waits
// while exports run back to back over a full ring with a matcher that folds
// case over the message and every attribute, as a keyword export does.
func BenchmarkBuffer_PushDuringFilteredExport(b *testing.B) {
	ring := logstream.NewBuffer(logstream.DefaultBufferSize)
	for i := 0; i < logstream.DefaultBufferSize; i++ {
		ring.Push(sampleEntry(i))
	}
	keyword := "no-such-keyword"
	match := func(e logstream.Entry) bool {
		if strings.Contains(strings.ToLower(e.Message), keyword) {
			return true
		}
		for _, v := range e.Attrs {
			if strings.Contains(strings.ToLower(v), keyword) {
				return true
			}
		}
		return false
	}

	var stop atomic.Bool
	var exporters sync.WaitGroup
	exporters.Add(1)
	go func() {
		defer exporters.Done()
		for !stop.Load() {
			ring.Snapshot(match, 0)
		}
	}()

	waits := make([]time.Duration, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		ring.Push(sampleEntry(i))
		waits[i] = time.Since(start)
	}
	b.StopTimer()
	stop.Store(true)
	exporters.Wait()

	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	b.ReportMetric(float64(waits[len(waits)*99/100].Microseconds()), "p99-us")
	b.ReportMetric(float64(waits[len(waits)-1].Microseconds()), "max-us")
}

func sampleEntry(i int) logstream.Entry {
	return logstream.Entry{
		Level:    logstream.LevelInfo,
		Message:  "request",
		TenantID: "acme",
		Attrs: map[string]string{
			"method":      "GET",
			"path":        fmt.Sprintf("/api/v1/content/posts/%d", i),
			"status":      "200",
			"duration_ms": "3",
			"request_id":  fmt.Sprintf("host/abcdef-%06d", i),
			"tenant_id":   "acme",
		},
	}
}
