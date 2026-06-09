package provider

import (
	"sync"
	"sync/atomic"
	"time"
)

// costTracker is the default in-memory CostTracker. Goroutine-safe via atomic
// counters for the fast path (increment) and a mutex for snapshot/reset.
type costTracker struct {
	mu            sync.Mutex
	byKey         map[string]*costBucket // key = "provider:operation"
	lastSnapshots map[string]time.Time   // key -> last snapshot captured (for range)
	firstSnapshot time.Time              // first Snapshot() call time
}

type costBucket struct {
	Ops      int64         `json:"ops"`
	BytesIn  int64         `json:"bytes_in"`
	BytesOut int64         `json:"bytes_out"`
	Latency  time.Duration `json:"latency_ns"`
	Errs     int64         `json:"errors"`
	LastErr  string        `json:"last_error,omitempty"`

	// Atomic counters for the hot path (Record).
	opsAtomic  atomic.Int64
	bytesInAt  atomic.Int64
	bytesOutAt atomic.Int64
	latencyAt  atomic.Int64 // nanoseconds
	errsAtomic atomic.Int64
}

// NewCostTracker creates an in-memory cost tracker.
func NewCostTracker() CostTracker {
	return &costTracker{
		byKey:         make(map[string]*costBucket),
		lastSnapshots: make(map[string]time.Time),
	}
}

func (t *costTracker) Record(cost OperationCost) {
	key := cost.Provider + ":" + cost.Operation
	t.mu.Lock()
	b, ok := t.byKey[key]
	if !ok {
		b = &costBucket{}
		t.byKey[key] = b
	}
	t.mu.Unlock()

	b.opsAtomic.Add(1)
	if cost.BytesIn > 0 {
		b.bytesInAt.Add(cost.BytesIn)
	}
	if cost.BytesOut > 0 {
		b.bytesOutAt.Add(cost.BytesOut)
	}
	b.latencyAt.Add(int64(cost.Latency))
	if cost.Error != "" {
		b.errsAtomic.Add(1)
		b.LastErr = cost.Error
	}
}

func (t *costTracker) Snapshot() ([]OperationCost, time.Time, time.Time) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.firstSnapshot.IsZero() {
		t.firstSnapshot = now
	}

	var since time.Time
	for _, ls := range t.lastSnapshots {
		if since.IsZero() || ls.Before(since) {
			since = ls
		}
	}
	if since.IsZero() {
		since = t.firstSnapshot
	}

	var out []OperationCost
	for key, b := range t.byKey {
		ops := b.opsAtomic.Swap(0)
		bytesIn := b.bytesInAt.Swap(0)
		bytesOut := b.bytesOutAt.Swap(0)
		latency := time.Duration(b.latencyAt.Swap(0))
		errs := b.errsAtomic.Swap(0)

		if ops == 0 {
			continue
		}

		provider, operation := splitKey(key)
		cost := OperationCost{
			Provider:  provider,
			Operation: operation,
			BytesIn:   bytesIn,
			BytesOut:  bytesOut,
			Latency:   latency,
			Timestamp: now,
		}
		if errs > 0 {
			cost.Error = b.LastErr
		}
		out = append(out, cost)

		t.lastSnapshots[key] = now
	}
	return out, since, now
}

func (t *costTracker) Total() []OperationCost {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []OperationCost
	for key, b := range t.byKey {
		ops := b.opsAtomic.Load()
		if ops == 0 {
			continue
		}
		provider, operation := splitKey(key)
		cost := OperationCost{
			Provider:  provider,
			Operation: operation,
			BytesIn:   b.bytesInAt.Load(),
			BytesOut:  b.bytesOutAt.Load(),
			Latency:   time.Duration(b.latencyAt.Load()),
			Timestamp: time.Now(),
		}
		if b.errsAtomic.Load() > 0 {
			cost.Error = b.LastErr
		}
		out = append(out, cost)
	}
	return out
}

func splitKey(key string) (provider, operation string) {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == ':' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
