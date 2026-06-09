// Real-time log tailing via SSE.
//
// LogTailer is a multi-subscriber fan-out broadcaster that attaches to the
// core's EnrichHandler. Every log record emitted through the handler
// is matched against each subscriber's TailFilter. Matching records are
// written to the subscriber's buffered channel for delivery over SSE.
//
// Design:
//   - Subscribers are identified by a monotonic string ID.
//   - Each subscriber has a buffered channel (default 256 entries).
//     Writes are non-blocking - if the channel is full the record is dropped.
//   - Subscribers can be paused/resumed via an atomic flag.
//   - A ring buffer (last 1024 entries) supports replay for late-joining
//     subscribers so they see the most recent log lines immediately.

package observability

import (
	"strings"
	"sync"
	"sync/atomic"
)

// TailFilter selects which log entries a subscriber receives.
// All fields are optional. An empty filter matches everything.
type TailFilter struct {
	// Levels is the set of slog level names to match (e.g. ["ERROR","WARN"]).
	// An empty slice matches all levels.
	Levels []string `json:"levels,omitempty"`

	// TenantID filters to a single tenant. Empty matches all.
	TenantID string `json:"tenant_id,omitempty"`

	// Plugin filters to a single plugin. Empty matches all.
	Plugin string `json:"plugin,omitempty"`

	// Query performs a case-insensitive substring match on the log message.
	// Empty matches all messages.
	Query string `json:"query,omitempty"`
}

// Match reports whether the filter matches the given LogEntry.
func (f TailFilter) Match(e LogEntry) bool {
	if len(f.Levels) > 0 {
		matched := false
		for _, lv := range f.Levels {
			if strings.EqualFold(e.Level, lv) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if f.TenantID != "" {
		tid, _ := e.Attrs["tenant_id"].(string)
		if tid != f.TenantID {
			return false
		}
	}

	if f.Plugin != "" {
		plugin, _ := e.Attrs["plugin"].(string)
		if plugin != f.Plugin {
			return false
		}
	}

	if f.Query != "" {
		if !strings.Contains(strings.ToLower(e.Message), strings.ToLower(f.Query)) {
			return false
		}
	}

	return true
}

// tailSubscriber (unexported)

type tailSubscriber struct {
	id     string
	ch     chan LogEntry
	filter TailFilter
	paused atomic.Bool
}

func (s *tailSubscriber) pause()         { s.paused.Store(true) }
func (s *tailSubscriber) resume()        { s.paused.Store(false) }
func (s *tailSubscriber) isPaused() bool { return s.paused.Load() }

// tailRingBuffer: bounded history for late-joiner replay

// Every goroutine that logs pushes here, so the buffer carries its own lock
// rather than the tailer's subscriber lock.
type tailRingBuffer struct {
	mu   sync.Mutex
	buf  []LogEntry
	head int
	size int
	cap  int
}

func newTailRingBuffer(cap int) *tailRingBuffer {
	if cap <= 0 {
		cap = 1024
	}
	return &tailRingBuffer{buf: make([]LogEntry, cap), cap: cap}
}

func (rb *tailRingBuffer) push(e LogEntry) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.buf[rb.head] = e
	rb.head = (rb.head + 1) % rb.cap
	if rb.size < rb.cap {
		rb.size++
	}
}

func (rb *tailRingBuffer) snapshot() []LogEntry {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.size == 0 {
		return nil
	}
	tail := (rb.head - rb.size + rb.cap) % rb.cap
	out := make([]LogEntry, 0, rb.size)
	for i := 0; i < rb.size; i++ {
		idx := (tail + i) % rb.cap
		out = append(out, rb.buf[idx])
	}
	return out
}

// LogTailer

// DefaultTailBufferSize is the subscriber channel buffer depth.
const DefaultTailBufferSize = 256

// DefaultTailRingSize is the number of recent entries kept for replay.
const DefaultTailRingSize = 1024

// Global log tailer: set at boot, and replaceable by a plugin.

var globalTailer atomic.Pointer[LogTailer]

// SetGlobalTailer stores the global log tailer.
func SetGlobalTailer(t *LogTailer) { globalTailer.Store(t) }

// GlobalTailer returns the global log tailer, or nil.
func GlobalTailer() *LogTailer { return globalTailer.Load() }

// LogTailer fans out enriched log entries to SSE subscribers.
type LogTailer struct {
	mu          sync.RWMutex
	subscribers map[string]*tailSubscriber
	nextID      atomic.Int64
	ring        *tailRingBuffer
}

// NewLogTailer creates a LogTailer ready for subscriptions.
func NewLogTailer() *LogTailer {
	return &LogTailer{
		subscribers: make(map[string]*tailSubscriber),
		ring:        newTailRingBuffer(DefaultTailRingSize),
	}
}

// Broadcast is called by the enrich handler for every log record.
// It matches each subscriber's filter and delivers to buffered channels.
// This MUST be non-blocking: slow/full subscribers have their entry dropped.
func (t *LogTailer) Broadcast(e LogEntry) {
	t.ring.push(e)

	t.mu.RLock()
	subs := make([]*tailSubscriber, 0, len(t.subscribers))
	for _, s := range t.subscribers {
		subs = append(subs, s)
	}
	t.mu.RUnlock()

	for _, s := range subs {
		if s.isPaused() {
			continue
		}
		if !s.filter.Match(e) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			// Channel full: drop.
		}
	}
}

// Subscribe adds a new subscriber with the given filter and returns its
// unique ID and receive channel.
func (t *LogTailer) Subscribe(filter TailFilter) (subID string, ch <-chan LogEntry) {
	id := formatTailID(t.nextID.Add(1))

	chImpl := make(chan LogEntry, DefaultTailBufferSize)
	s := &tailSubscriber{
		id:     id,
		ch:     chImpl,
		filter: filter,
	}

	t.mu.Lock()
	t.subscribers[id] = s
	t.mu.Unlock()

	return id, chImpl
}

// Unsubscribe removes a subscriber by ID.
func (t *LogTailer) Unsubscribe(subID string) {
	t.mu.Lock()
	delete(t.subscribers, subID)
	t.mu.Unlock()
}

// Pause stops delivery to a subscriber, for a caller entitled to it.
//
// tenantID is the caller's scope, and an empty one is the platform scope that
// reaches every stream. A caller inside a tenant reaches only the streams
// opened under it.
//
// The check is here rather than in the caller because a subscriber id is a
// counter rendered in base62, so the ids are 1, 2, 3 and so on. Without it a
// tenant admin could walk the sequence and silence another tenant's live
// stream, and the operator watching that stream would see it go quiet with no
// error anywhere.
func (t *LogTailer) Pause(subID, tenantID string) bool {
	s, ok := t.subscriberFor(subID, tenantID)
	if !ok {
		return false
	}
	s.pause()
	return true
}

// subscriberFor resolves a subscriber the caller is entitled to act on, and
// answers the same "not found" for a stream that does not exist and one that
// belongs to another tenant. Telling those apart would turn the counter into
// a census of live streams on the instance.
func (t *LogTailer) subscriberFor(subID, tenantID string) (*tailSubscriber, bool) {
	t.mu.RLock()
	s, ok := t.subscribers[subID]
	t.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if tenantID != "" && s.filter.TenantID != tenantID {
		return nil, false
	}
	return s, true
}

// Resume restarts delivery to a subscriber, for a caller entitled to it.
// Scoped the same way Pause is, and for the same reason.
func (t *LogTailer) Resume(subID, tenantID string) bool {
	s, ok := t.subscriberFor(subID, tenantID)
	if !ok {
		return false
	}
	s.resume()
	return true
}

// Recent returns the last N log entries from the ring buffer, optionally
// filtered. Used for the export endpoint and for initial catch-up on connect.
func (t *LogTailer) Recent(filter TailFilter, max int) []LogEntry {
	entries := t.ring.snapshot()
	if max <= 0 || max > len(entries) {
		max = len(entries)
	}

	var result []LogEntry
	for i := len(entries) - 1; i >= 0 && len(result) < max; i-- {
		e := entries[i]
		if filter.Match(e) {
			result = append(result, e)
		}
	}

	// Reverse to chronological order.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// SubscriberCount returns the current number of active subscribers.
func (t *LogTailer) SubscriberCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.subscribers)
}

// Helpers

const tailCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func formatTailID(n int64) string {
	var buf [12]byte
	i := len(buf)
	for {
		i--
		buf[i] = tailCharset[n%62]
		n /= 62
		if n == 0 {
			break
		}
	}
	return string(buf[i:])
}
