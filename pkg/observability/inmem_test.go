package observability

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messagesOf projects a slice of LogEntry down to their messages for assertions.
func messagesOf(entries []LogEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Message
	}
	return out
}

// logtailer.go: TailFilter.Match

func TestTailFilter_Match(t *testing.T) {
	entry := LogEntry{
		Level:   "ERROR",
		Message: "database connection Failed",
		Attrs:   map[string]any{"tenant_id": "acme", "plugin": "content"},
	}

	tests := []struct {
		name   string
		filter TailFilter
		want   bool
	}{
		{"empty matches all", TailFilter{}, true},
		{"level match", TailFilter{Levels: []string{"error"}}, true}, // case-insensitive
		{"level miss", TailFilter{Levels: []string{"INFO", "WARN"}}, false},
		{"tenant match", TailFilter{TenantID: "acme"}, true},
		{"tenant miss", TailFilter{TenantID: "other"}, false},
		{"plugin match", TailFilter{Plugin: "content"}, true},
		{"plugin miss", TailFilter{Plugin: "media"}, false},
		{"query match (case-insensitive substring)", TailFilter{Query: "connection failed"}, true},
		{"query miss", TailFilter{Query: "timeout"}, false},
		{"combined all match", TailFilter{Levels: []string{"ERROR"}, TenantID: "acme", Plugin: "content", Query: "database"}, true},
		{"combined one miss", TailFilter{Levels: []string{"ERROR"}, TenantID: "wrong"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.filter.Match(entry))
		})
	}
}

// logtailer.go: ring buffer

func TestTailRingBuffer(t *testing.T) {
	// A non-positive capacity falls back to the default.
	assert.Equal(t, DefaultTailRingSize, newTailRingBuffer(0).cap)
	assert.Equal(t, DefaultTailRingSize, newTailRingBuffer(-5).cap)

	rb := newTailRingBuffer(3)
	assert.Nil(t, rb.snapshot(), "empty buffer snapshots to nil")

	rb.push(LogEntry{Message: "a"})
	rb.push(LogEntry{Message: "b"})
	assert.Equal(t, []string{"a", "b"}, messagesOf(rb.snapshot()))

	// Overflow: oldest entries are evicted, order preserved.
	rb.push(LogEntry{Message: "c"})
	rb.push(LogEntry{Message: "d"})
	assert.Equal(t, []string{"b", "c", "d"}, messagesOf(rb.snapshot()))
}

// logtailer.go: LogTailer broadcast / subscribe lifecycle

func TestLogTailer_SubscribeBroadcast(t *testing.T) {
	tl := NewLogTailer()
	assert.Equal(t, 0, tl.SubscriberCount())

	id, ch := tl.Subscribe(TailFilter{Levels: []string{"ERROR"}})
	assert.Equal(t, 1, tl.SubscriberCount())

	// Non-matching entry is filtered out. Matching entry is delivered.
	tl.Broadcast(LogEntry{Level: "INFO", Message: "ignored"})
	tl.Broadcast(LogEntry{Level: "ERROR", Message: "kept"})

	select {
	case e := <-ch:
		assert.Equal(t, "kept", e.Message)
	default:
		t.Fatal("expected a delivered entry")
	}
	assert.Empty(t, ch, "only the matching entry should have been delivered")

	// Pause suppresses delivery. Resume restores it.
	require.True(t, tl.Pause(id, ""))
	tl.Broadcast(LogEntry{Level: "ERROR", Message: "while-paused"})
	assert.Empty(t, ch)
	require.True(t, tl.Resume(id, ""))
	tl.Broadcast(LogEntry{Level: "ERROR", Message: "after-resume"})
	require.Len(t, ch, 1)
	assert.Equal(t, "after-resume", (<-ch).Message)

	// Pause/Resume on an unknown subscriber report false.
	assert.False(t, tl.Pause("nonexistent", ""))
	assert.False(t, tl.Resume("nonexistent", ""))

	tl.Unsubscribe(id)
	assert.Equal(t, 0, tl.SubscriberCount())
}

func TestLogTailer_BroadcastDropsWhenFull(t *testing.T) {
	tl := NewLogTailer()
	_, ch := tl.Subscribe(TailFilter{}) // matches all

	// Nobody drains the channel. Excess entries are dropped, never block.
	for i := 0; i < DefaultTailBufferSize+16; i++ {
		tl.Broadcast(LogEntry{Message: "x"})
	}
	assert.Len(t, ch, DefaultTailBufferSize, "channel caps at its buffer size")
}

func TestLogTailer_Recent(t *testing.T) {
	tl := NewLogTailer()
	tl.Broadcast(LogEntry{Level: "INFO", Message: "first"})
	tl.Broadcast(LogEntry{Level: "ERROR", Message: "second"})
	tl.Broadcast(LogEntry{Level: "INFO", Message: "third"})

	// No filter, no cap -> all entries in chronological order.
	assert.Equal(t, []string{"first", "second", "third"}, messagesOf(tl.Recent(TailFilter{}, 0)))

	// Filtered to a level.
	assert.Equal(t, []string{"second"}, messagesOf(tl.Recent(TailFilter{Levels: []string{"ERROR"}}, 0)))

	// Capped to the most recent N, still chronological.
	assert.Equal(t, []string{"second", "third"}, messagesOf(tl.Recent(TailFilter{}, 2)))
}

func TestGlobalTailer(t *testing.T) {
	orig := GlobalTailer()
	t.Cleanup(func() { SetGlobalTailer(orig) })

	tl := NewLogTailer()
	SetGlobalTailer(tl)
	assert.Same(t, tl, GlobalTailer())
}

func TestFormatTailID(t *testing.T) {
	// Base-62 encoding over "a..zA..Z0..9".
	assert.Equal(t, "a", formatTailID(0))
	assert.Equal(t, "b", formatTailID(1))
	assert.Equal(t, "9", formatTailID(61))
	assert.Equal(t, "ba", formatTailID(62))
}

// A subscriber id is a counter rendered in base62, so the ids are 1, 2, 3 and
// so on. Without a scope check a tenant admin could walk the sequence and
// silence another tenant's live stream, and the operator watching it would see
// it go quiet with no error anywhere.
func TestLogTailer_PauseAndResumeStayInsideTheTenant(t *testing.T) {
	tl := NewLogTailer()
	id, _ := tl.Subscribe(TailFilter{TenantID: "acme"})

	assert.False(t, tl.Pause(id, "other"),
		"a caller in another tenant must not reach this stream")
	assert.False(t, tl.Resume(id, "other"),
		"a caller in another tenant must not reach this stream")

	assert.True(t, tl.Pause(id, "acme"), "the owning tenant reaches its own stream")
	assert.True(t, tl.Resume(id, "acme"))

	assert.True(t, tl.Pause(id, ""), "the platform scope reaches every stream")
	assert.True(t, tl.Resume(id, ""))

	// A stream that does not exist and one that belongs to somebody else
	// answer the same, so the counter cannot be walked to count live streams.
	assert.False(t, tl.Pause("nope", "acme"))
}
