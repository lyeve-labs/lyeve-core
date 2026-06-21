package engine

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// TrackerConfig configures the goroutine lifecycle tracker.
type TrackerConfig struct {
	// LeakThreshold logs a warning when a tracked goroutine runs longer than this.
	// Zero disables leak detection.
	LeakThreshold time.Duration
	// MaxGoroutines logs a warning when total goroutines (Go runtime count)
	// exceeds this threshold. Zero disables.
	MaxGoroutines int
}

// DefaultTrackerConfig returns a safe default.
func DefaultTrackerConfig() TrackerConfig {
	return TrackerConfig{LeakThreshold: 120 * time.Second, MaxGoroutines: 50000}
}

// TrackedFunc is a goroutine function that receives a context canceled
// during shutdown.
type TrackedFunc func(ctx context.Context)

// goroutineEntry tracks a single spawned goroutine.
type goroutineEntry struct {
	id      int64
	owner   string
	name    string
	started time.Time
}

// OwnerEngine is the owner recorded for a goroutine the engine itself
// starts. A plugin's goroutines carry the plugin's name: the host hands
// each plugin a view of the tracker bound to it, so a leak in the snapshot
// names who started it.
const OwnerEngine = "engine"

// trackerState is the shared half of a GoroutineTracker. Every view made
// by ForOwner points at the same one, so the entries, the shutdown context
// and the wait group are one set however many owners record into them.
type trackerState struct {
	config  TrackerConfig
	mu      sync.Mutex
	entries map[int64]goroutineEntry
	nextID  atomic.Int64
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// GoroutineTracker provides lifecycle tracking, leak detection, and
// observable shutdown for goroutines. Every long-lived goroutine in the
// engine should be spawned through Tracker.Go instead of a raw go func().
//
// A tracker is a view over shared state bound to one owner. The runtime
// makes the root, owned by the engine, and each plugin receives ForOwner
// of it through its scoped host, so Go needs no owner argument and a plugin
// cannot record under another name.
type GoroutineTracker struct {
	*trackerState
	owner string
}

// NewGoroutineTracker creates a tracker owned by the engine. Call Shutdown
// to cancel all tracked goroutines and wait for them to finish.
func NewGoroutineTracker(cfg TrackerConfig) *GoroutineTracker {
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.LeakThreshold == 0 && cfg.MaxGoroutines == 0 {
		cfg = DefaultTrackerConfig()
	}
	t := &GoroutineTracker{
		trackerState: &trackerState{
			config:  cfg,
			entries: make(map[int64]goroutineEntry),
			ctx:     ctx,
			cancel:  cancel,
		},
		owner: OwnerEngine,
	}
	go t.monitorLoop()
	return t
}

// ForOwner returns a view of the same tracker that records owner on every
// goroutine it starts. An empty owner returns the receiver unchanged.
func (t *GoroutineTracker) ForOwner(owner string) *GoroutineTracker {
	if owner == "" || owner == t.owner {
		return t
	}
	return &GoroutineTracker{trackerState: t.trackerState, owner: owner}
}

// Owner returns the name this view records.
func (t *GoroutineTracker) Owner() string { return t.owner }

// Go spawns a tracked goroutine. Returns an ID for later reference.
// When the tracker has been shut down (ctx canceled), Go returns 0 and
// does not spawn the goroutine: this prevents races between concurrent
// Go calls and Shutdown that would leak goroutines past Wait.
func (t *GoroutineTracker) Go(name string, fn TrackedFunc) int64 {
	// Fast check: if already shut down, reject immediately.
	select {
	case <-t.ctx.Done():
		return 0
	default:
	}

	id := t.nextID.Add(1)
	owner := t.owner
	t.mu.Lock()
	t.entries[id] = goroutineEntry{id: id, owner: owner, name: name, started: time.Now()}
	t.mu.Unlock()

	t.wg.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("tracked goroutine panicked",
					"id", id, "owner", owner, "name", name, "panic", fmt.Sprintf("%v", r))
			}
			t.mu.Lock()
			delete(t.entries, id)
			t.mu.Unlock()
			t.wg.Done()
		}()
		fn(t.ctx)
	}()
	return id
}

// Wait blocks until all tracked goroutines have finished. Call after
// canceling the tracker context (via Shutdown).
func (t *GoroutineTracker) Wait(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("goroutine tracker: shutdown timed out after %v", timeout)
	}
}

// Shutdown cancels all tracked goroutines and waits for them to finish.
func (t *GoroutineTracker) Shutdown(timeout time.Duration) error {
	t.cancel()
	return t.Wait(timeout)
}

// Count returns the number of currently tracked goroutines.
func (t *GoroutineTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// Snapshot returns a summary suitable for the /debug/goroutines endpoint.
func (t *GoroutineTracker) Snapshot() GoroutineSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	byName := make(map[string]int)
	byOwner := make(map[string]OwnerCount)
	now := time.Now()
	for _, e := range t.entries {
		byName[e.name]++
		oc := byOwner[e.owner]
		oc.Total++
		if age := now.Sub(e.started); age > oc.oldest {
			oc.oldest = age
			oc.Oldest = age.Truncate(time.Millisecond).String()
		}
		byOwner[e.owner] = oc
	}

	return GoroutineSnapshot{
		Total:         len(t.entries),
		RuntimeTotal:  runtime.NumGoroutine(),
		BySource:      byName,
		ByOwner:       byOwner,
		MaxGoroutines: t.config.MaxGoroutines,
		LeakThreshold: t.config.LeakThreshold.String(),
	}
}

// OwnerCount is one owner's row in a snapshot: how many of its goroutines
// are live and how long the oldest has run. A leak shows as a row whose
// total climbs and whose oldest keeps growing.
type OwnerCount struct {
	Total  int    `json:"total"`
	Oldest string `json:"oldest"`

	oldest time.Duration
}

// GoroutineSnapshot is returned by Tracker.Snapshot.
type GoroutineSnapshot struct {
	Total        int            `json:"total"`
	RuntimeTotal int            `json:"runtime_total"`
	BySource     map[string]int `json:"by_source"`
	// ByOwner keys are plugin names, or OwnerEngine for the engine's own.
	ByOwner       map[string]OwnerCount `json:"by_owner"`
	MaxGoroutines int                   `json:"max_goroutines"`
	LeakThreshold string                `json:"leak_threshold"`
}

func (t *GoroutineTracker) monitorLoop() {
	if t.config.LeakThreshold == 0 && t.config.MaxGoroutines == 0 {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			t.checkLeaks()
		}
	}
}

func (t *GoroutineTracker) checkLeaks() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	for _, e := range t.entries {
		age := now.Sub(e.started)
		if t.config.LeakThreshold > 0 && age > t.config.LeakThreshold {
			slog.Warn("goroutine may be leaking",
				"id", e.id, "owner", e.owner, "name", e.name, "age", age.String())
		}
	}

	if t.config.MaxGoroutines > 0 {
		rt := runtime.NumGoroutine()
		if rt > t.config.MaxGoroutines {
			slog.Warn("high goroutine count",
				"runtime_total", rt, "max", t.config.MaxGoroutines,
				"tracked", len(t.entries))
		}
	}
}
