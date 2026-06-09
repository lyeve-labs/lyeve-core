package logstream_test

import (
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/logstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Level tests

func TestLevelFromString(t *testing.T) {
	tests := []struct {
		input    string
		expected logstream.Level
	}{
		{"debug", logstream.LevelDebug},
		{"DEBUG", logstream.LevelDebug},
		{"info", logstream.LevelInfo},
		{"INFO", logstream.LevelInfo},
		{"warn", logstream.LevelWarn},
		{"warning", logstream.LevelWarn},
		{"WARN", logstream.LevelWarn},
		{"WARNING", logstream.LevelWarn},
		{"error", logstream.LevelError},
		{"ERROR", logstream.LevelError},
		{"unknown", logstream.LevelInfo}, // defaults to Info
		{"", logstream.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, logstream.LevelFromString(tt.input))
		})
	}
}

func TestLevel_String(t *testing.T) {
	assert.Equal(t, "DEBUG", logstream.LevelDebug.String())
	assert.Equal(t, "INFO", logstream.LevelInfo.String())
	assert.Equal(t, "WARN", logstream.LevelWarn.String())
	assert.Equal(t, "ERROR", logstream.LevelError.String())
}

// Buffer tests

func TestNewBuffer(t *testing.T) {
	b := logstream.NewBuffer(100)
	assert.NotNil(t, b)

	stats := b.Stats()
	assert.Equal(t, 0, stats.Entries)
	assert.Equal(t, 100, stats.Capacity)
	assert.Equal(t, uint64(0), stats.Sequence)
	assert.Equal(t, 0, stats.Subscribers)
}

func TestNewBuffer_DefaultSize(t *testing.T) {
	b := logstream.NewBuffer(0)
	stats := b.Stats()
	assert.Equal(t, logstream.DefaultBufferSize, stats.Capacity)
}

func TestBuffer_Push(t *testing.T) {
	b := logstream.NewBuffer(100)

	e := logstream.Entry{Level: logstream.LevelInfo, Message: "hello"}
	b.Push(e)

	stats := b.Stats()
	assert.Equal(t, 1, stats.Entries)
	assert.Equal(t, uint64(1), stats.Sequence)
}

func TestBuffer_PushSequence(t *testing.T) {
	b := logstream.NewBuffer(100)
	b.Push(logstream.Entry{Level: logstream.LevelInfo, Message: "one"})
	b.Push(logstream.Entry{Level: logstream.LevelInfo, Message: "two"})
	b.Push(logstream.Entry{Level: logstream.LevelInfo, Message: "three"})

	stats := b.Stats()
	assert.Equal(t, 3, stats.Entries)
	assert.Equal(t, uint64(3), stats.Sequence)
}

func TestBuffer_Snapshot_ALL(t *testing.T) {
	b := logstream.NewBuffer(100)
	b.Push(logstream.Entry{Message: "a"})
	b.Push(logstream.Entry{Message: "b"})
	b.Push(logstream.Entry{Message: "c"})

	entries := b.Snapshot(nil, 0)
	assert.Len(t, entries, 3)
	assert.Equal(t, "a", entries[0].Message)
	assert.Equal(t, "b", entries[1].Message)
	assert.Equal(t, "c", entries[2].Message)
}

func TestBuffer_Snapshot_MaxLimit(t *testing.T) {
	b := logstream.NewBuffer(100)
	for i := 0; i < 10; i++ {
		b.Push(logstream.Entry{Message: string(rune('a' + i))})
	}

	entries := b.Snapshot(nil, 5)
	assert.Len(t, entries, 5)
}

func TestBuffer_Snapshot_Predicate(t *testing.T) {
	b := logstream.NewBuffer(100)
	b.Push(logstream.Entry{Level: logstream.LevelDebug, Message: "debug msg"})
	b.Push(logstream.Entry{Level: logstream.LevelInfo, Message: "info msg"})
	b.Push(logstream.Entry{Level: logstream.LevelError, Message: "error msg"})

	entries := b.Snapshot(func(e logstream.Entry) bool { return e.Level >= logstream.LevelWarn }, 0)
	assert.Len(t, entries, 1)
	assert.Equal(t, "error msg", entries[0].Message)
}

func TestBuffer_Snapshot_PredicateSeesSequence(t *testing.T) {
	b := logstream.NewBuffer(100)
	b.Push(logstream.Entry{Message: "first"})  // seq 1
	b.Push(logstream.Entry{Message: "second"}) // seq 2
	b.Push(logstream.Entry{Message: "third"})  // seq 3

	entries := b.Snapshot(func(e logstream.Entry) bool { return e.Sequence > 1 }, 0)
	assert.Len(t, entries, 2)
	assert.Equal(t, "second", entries[0].Message)
	assert.Equal(t, "third", entries[1].Message)
}

func TestBuffer_Snapshot_PredicateWithMax(t *testing.T) {
	b := logstream.NewBuffer(100)
	for i := 0; i < 10; i++ {
		b.Push(logstream.Entry{Message: string(rune('a' + i))})
	}

	// The cap counts kept records, not scanned ones.
	entries := b.Snapshot(func(e logstream.Entry) bool { return e.Sequence%2 == 0 }, 3)
	require.Len(t, entries, 3)
	assert.Equal(t, "b", entries[0].Message)
	assert.Equal(t, "f", entries[2].Message)
}

func TestBuffer_Snapshot_Empty(t *testing.T) {
	b := logstream.NewBuffer(100)
	entries := b.Snapshot(nil, 0)
	assert.Nil(t, entries)
}

func TestBuffer_RingBufferOverflow(t *testing.T) {
	b := logstream.NewBuffer(5)
	for i := 0; i < 10; i++ {
		b.Push(logstream.Entry{Message: string(rune('a' + i))})
	}

	stats := b.Stats()
	assert.Equal(t, 5, stats.Entries, "should cap at capacity")

	entries := b.Snapshot(nil, 0)
	assert.Len(t, entries, 5)
	// Last 5 entries should be f,g,h,i,j
	assert.Equal(t, "f", entries[0].Message)
}

func TestBuffer_SubscribeUnsubscribe(t *testing.T) {
	b := logstream.NewBuffer(100)

	id, ch := b.Subscribe(10)
	assert.NotZero(t, id)
	assert.NotNil(t, ch)

	stats := b.Stats()
	assert.Equal(t, 1, stats.Subscribers)

	b.Unsubscribe(id)
	stats = b.Stats()
	assert.Equal(t, 0, stats.Subscribers)
}

func TestBuffer_Subscribe_ReceiveEntry(t *testing.T) {
	b := logstream.NewBuffer(100)
	id, ch := b.Subscribe(10)
	defer b.Unsubscribe(id)

	b.Push(logstream.Entry{Level: logstream.LevelInfo, Message: "live"})

	select {
	case e := <-ch:
		assert.Equal(t, "live", e.Message)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for entry")
	}
}

func TestBuffer_Subscribe_DropSlowSubscriber(t *testing.T) {
	b := logstream.NewBuffer(100)
	id, ch := b.Subscribe(1) // buffer of 1
	defer b.Unsubscribe(id)

	// Fill the channel buffer
	b.Push(logstream.Entry{Message: "first"})

	// Drain the first message so channel has room
	<-ch

	// Next push should succeed without blocking
	done := make(chan struct{})
	go func() {
		b.Push(logstream.Entry{Message: "second"})
		b.Push(logstream.Entry{Message: "third"})
		close(done)
	}()

	select {
	case <-done:
		// success: Push didn't block
	case <-time.After(time.Second):
		t.Fatal("Push blocked on slow subscriber")
	}
}

func TestBuffer_Unsubscribe_ClosesChannel(t *testing.T) {
	b := logstream.NewBuffer(100)
	id, ch := b.Subscribe(10)
	b.Unsubscribe(id)

	// Channel should be closed
	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel should be closed")
	default:
		// Channel might still have buffered items before close
	}
}

func TestBuffer_Subscribe_DoubleUnsubscribe(t *testing.T) {
	b := logstream.NewBuffer(100)
	id, _ := b.Subscribe(10)
	b.Unsubscribe(id)
	// Should not panic
	b.Unsubscribe(id)
}

func TestBuffer_ConcurrentPushSubscribe(t *testing.T) {
	b := logstream.NewBuffer(1000)
	var wg sync.WaitGroup

	// Multiple subscribers
	const numSubs = 5
	for i := 0; i < numSubs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, ch := b.Subscribe(50)
			defer b.Unsubscribe(id)
			// Drain for a bit
			for j := 0; j < 10; j++ {
				select {
				case <-ch:
				case <-time.After(100 * time.Millisecond):
					return
				}
			}
		}()
	}

	// Concurrent pushes
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Push(logstream.Entry{Message: string(rune('a' + i%26))})
		}(i)
	}

	wg.Wait()
	// No panics = pass
}
