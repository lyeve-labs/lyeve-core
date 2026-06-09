package logstream

import (
	"sync"
	"sync/atomic"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The buffer is the ring a plugin reads through the core.LogRing role.
var _ core.LogRing = (*Buffer)(nil)

// DefaultBufferSize is the default number of log entries kept in the ring buffer.
// 10,000 entries at ~500 bytes each ≈ 5 MB memory.
const DefaultBufferSize = 10000

// Buffer is a lock-free-ish ring buffer of log entries with a broadcast
// mechanism for SSE clients. Writers push entries. Readers subscribe and
// receive a channel of new entries.
type Buffer struct {
	mu      sync.RWMutex
	entries []Entry
	head    int    // next write position
	size    int    // current number of entries (capped at cap)
	cap     int    // maximum entries
	next    uint64 // monotonically increasing sequence counter

	subsMu sync.RWMutex
	subs   map[uint64]chan Entry // subscriber ID -> channel
	subID  atomic.Uint64
}

// NewBuffer creates a ring buffer with the given capacity.
// If cap <= 0, DefaultBufferSize is used.
func NewBuffer(cap int) *Buffer {
	if cap <= 0 {
		cap = DefaultBufferSize
	}
	return &Buffer{
		entries: make([]Entry, cap),
		cap:     cap,
		subs:    make(map[uint64]chan Entry),
	}
}

// Push adds an entry to the ring buffer and broadcasts it to all subscribers.
func (b *Buffer) Push(e Entry) {
	b.mu.Lock()
	seq := atomic.AddUint64(&b.next, 1)
	e.Sequence = seq

	b.entries[b.head] = e
	b.head = (b.head + 1) % b.cap
	if b.size < b.cap {
		b.size++
	}
	b.mu.Unlock()

	// Broadcast to subscribers without holding the main lock.
	b.subsMu.RLock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Subscriber is slow. Drop the entry for this subscriber.
			// The subscriber can catch up via the export endpoint.
		}
	}
	b.subsMu.RUnlock()
}

// Subscribe returns a new subscriber channel for real-time log entries.
// The caller is responsible for calling Unsubscribe when done.
// channelBuf is the buffer size for the subscriber's channel.
func (b *Buffer) Subscribe(channelBuf int) (id uint64, ch <-chan Entry) {
	id = b.subID.Add(1)
	c := make(chan Entry, channelBuf)

	b.subsMu.Lock()
	b.subs[id] = c
	b.subsMu.Unlock()

	return id, c
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *Buffer) Unsubscribe(id uint64) {
	b.subsMu.Lock()
	ch, ok := b.subs[id]
	if ok {
		delete(b.subs, id)
		close(ch)
	}
	b.subsMu.Unlock()
}

// Snapshot returns a copy of all entries in the buffer, from oldest to newest,
// that match accepts. If match is nil, all entries are returned.
// max limits the result count. 0 means no limit.
func (b *Buffer) Snapshot(match func(Entry) bool, max int) []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.size == 0 {
		return nil
	}

	result := make([]Entry, 0, b.size)
	start := b.head - b.size
	if start < 0 {
		start += b.cap
	}

	for i := 0; i < b.size; i++ {
		idx := (start + i) % b.cap
		e := b.entries[idx]
		if match != nil && !match(e) {
			continue
		}
		result = append(result, e)
		if max > 0 && len(result) >= max {
			break
		}
	}

	return result
}

// Stats returns the current buffer statistics.
func (b *Buffer) Stats() BufferStats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	b.subsMu.RLock()
	subCount := len(b.subs)
	b.subsMu.RUnlock()

	return BufferStats{
		Entries:     b.size,
		Capacity:    b.cap,
		Sequence:    atomic.LoadUint64(&b.next),
		Subscribers: subCount,
	}
}
