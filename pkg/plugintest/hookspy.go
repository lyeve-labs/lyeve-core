package plugintest

import (
	"context"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// HookSpy records hook events and subscriptions for test assertions.
// It implements core.HookBus. Every Publish and Subscribe call is
// recorded for inspection. Safe for concurrent use.
type HookSpy struct {
	mu sync.RWMutex

	// Events records every published event in order.
	Events []core.Event

	// Subscriptions records every subscription call.
	Subscriptions []SubscriptionRecord

	// Handle, when set, is called for every published event after recording.
	Handle func(ctx context.Context, event core.Event) error

	// PublishError, when non-nil, is returned by every Publish call.
	PublishError error

	subID int
}

// SubscriptionRecord captures one Subscribe call.
type SubscriptionRecord struct {
	Schema string
	Event  core.EventType
}

// NewHookSpy returns a ready-to-use HookSpy.
func NewHookSpy() *HookSpy {
	return &HookSpy{}
}

// Subscribe records the subscription and returns a no-op Subscription.
func (h *HookSpy) Subscribe(schema string, event core.EventType, handler core.EventHandler) core.Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Subscriptions = append(h.Subscriptions, SubscriptionRecord{Schema: schema, Event: event})
	h.subID++
	id := h.subID

	if h.Handle != nil {
		_ = id // Handle is invoked during Publish, not at Subscribe time
	}

	return &spySubscription{spy: h, id: id}
}

// On records the system event subscription and returns a no-op Subscription.
func (h *HookSpy) On(event string, handler core.SystemEventHandler) core.Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subID++
	return &spySubscription{spy: h, id: h.subID}
}

// Publish records the event and returns PublishError. When Handle is set,
// it is called with the original event after recording.
func (h *HookSpy) Publish(ctx context.Context, event core.Event) error {
	h.mu.Lock()
	ev := copyEvent(event) // copy so caller mutations don't affect the record
	h.Events = append(h.Events, ev)
	err := h.PublishError
	h.mu.Unlock()

	if err != nil {
		return err
	}
	if h.Handle != nil {
		return h.Handle(ctx, event)
	}
	return nil
}

// Assertions

// AssertPublished fails if no event of the given (type, schema) pair was published.
func (h *HookSpy) AssertPublished(t T, eventType core.EventType, schema string) {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, e := range h.Events {
		if e.Type == eventType && e.Schema == schema {
			return
		}
	}
	t.Errorf("expected event %s on schema %q to be published, but it was not", eventType, schema)
}

// AssertNotPublished fails if an event of the given (type, schema) pair was published.
func (h *HookSpy) AssertNotPublished(t T, eventType core.EventType, schema string) {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, e := range h.Events {
		if e.Type == eventType && e.Schema == schema {
			t.Errorf("expected event %s on schema %q NOT to be published, but it was", eventType, schema)
			return
		}
	}
}

// AssertSubscribed fails if no subscription for (type, schema) was made.
func (h *HookSpy) AssertSubscribed(t T, eventType core.EventType, schema string) {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.Subscriptions {
		if s.Event == eventType && s.Schema == schema {
			return
		}
	}
	t.Errorf("expected subscription %s on schema %q, but none was made", eventType, schema)
}

// AssertEventCount fails if the published event count does not match want.
func (h *HookSpy) AssertEventCount(t T, want int) {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.Events) != want {
		t.Errorf("expected %d events, got %d", want, len(h.Events))
	}
}

// Published returns a copy of all recorded events.
func (h *HookSpy) Published() []core.Event {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]core.Event, len(h.Events))
	copy(out, h.Events)
	return out
}

// PublishedCount returns the number of recorded events.
func (h *HookSpy) PublishedCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.Events)
}

// Reset clears all recorded events and subscriptions. Not safe for concurrent use.
func (h *HookSpy) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Events = nil
	h.Subscriptions = nil
	h.subID = 0
}

// spySubscription

type spySubscription struct {
	spy *HookSpy
	id  int
}

// Unsubscribe is a no-op for the spy subscription.
func (s *spySubscription) Unsubscribe() {}

// helpers

func copyEvent(e core.Event) core.Event {
	deep := e
	if e.Data != nil {
		deep.Data = make(map[string]any, len(e.Data))
		for k, v := range e.Data {
			deep.Data[k] = v
		}
	}
	if e.OldData != nil {
		deep.OldData = make(map[string]any, len(e.OldData))
		for k, v := range e.OldData {
			deep.OldData[k] = v
		}
	}
	return deep
}

// T is the minimal testing interface used by plugintest assertions.
// *testing.T, *testing.B, and *testing.F all satisfy this interface.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}
