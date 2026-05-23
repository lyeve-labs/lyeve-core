package core

import (
	"context"
	"time"
)

// HookBus allows plugins to subscribe to content lifecycle events
// and system-level events like "license.changed".
type HookBus interface {
	// Subscribe registers a handler for the given event type and schema.
	// Use schema "*" to receive events for all schemas.
	// Returns a Subscription that must be closed when done.
	Subscribe(schema string, event EventType, handler EventHandler) Subscription

	// On registers a handler for a named system event (e.g. "license.changed").
	// System events are not schema-scoped and carry a HookEvent payload.
	// Returns a Subscription that can be used to unsubscribe.
	On(event string, handler SystemEventHandler) Subscription
}

// SystemEventHandler is called when a named system event fires.
// The HookEvent carries the event name and any associated data.
type SystemEventHandler func(ctx context.Context, event HookEvent) error

// HookEvent represents a system-level event published on the HookBus.
type HookEvent struct {
	// Name is the event name (e.g. "license.changed").
	Name string `json:"name"`
	// Data carries optional key-value data associated with the event.
	Data map[string]any `json:"data,omitempty"`
}

// HookPublisher allows publishing lifecycle events. This is a separate
// interface so only trusted callers get publish capability.
type HookPublisher interface {
	// Publish fires an event to all registered handlers.
	// Used internally by the engine. Plugins typically only Subscribe.
	Publish(ctx context.Context, event Event) error
}

// Subscription represents an active hook subscription.
type Subscription interface {
	// Unsubscribe removes this handler from the registry.
	Unsubscribe()
}

// EventHandler is called when a matching lifecycle event fires.
// Returning a non-nil error from a Before* handler aborts the operation (HTTP 422).
// Errors from After* handlers are logged and ignored.
type EventHandler func(ctx context.Context, event Event) error

// EventType identifies a content lifecycle hook point.
type EventType string

const (
	// BeforeCreate fires before a content entry is created.
	BeforeCreate EventType = "before_create"
	// AfterCreate fires after a content entry is created.
	AfterCreate EventType = "after_create"
	// BeforeUpdate fires before a content entry is updated.
	BeforeUpdate EventType = "before_update"
	// AfterUpdate fires after a content entry is updated.
	AfterUpdate EventType = "after_update"
	// BeforeDelete fires before a content entry is deleted.
	BeforeDelete EventType = "before_delete"
	// AfterDelete fires after a content entry is deleted.
	AfterDelete EventType = "after_delete"
)

// Event represents a hook-bus event published during content lifecycle
// operations (create, update, delete) and integration delivery attempts.
type Event struct {
	Type       EventType      `json:"type"`
	Schema     string         `json:"schema"`
	Data       map[string]any `json:"data"`
	OldData    map[string]any `json:"old_data,omitempty"`
	RecordID   string         `json:"record_id,omitempty"`
	TenantID   string         `json:"tenant_id,omitempty"`
	InstanceID string         `json:"instance_id,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
	Source     string         `json:"source,omitempty"` // optional origin annotation
}

// SourcePlugin is the canonical Source value for plugin-published events.
const SourcePlugin = "plugin"

// NoopHookBus is a HookBus that silently discards all subscriptions and events.
// Useful as a stub in tests or for plugins that don't need lifecycle hooks.
type NoopHookBus struct{}

func (NoopHookBus) Subscribe(string, EventType, EventHandler) Subscription { return NoopSubscription{} }
func (NoopHookBus) On(string, SystemEventHandler) Subscription             { return NoopSubscription{} }

// NoopSubscription is a Subscription that does nothing on Unsubscribe.
type NoopSubscription struct{}

func (NoopSubscription) Unsubscribe() {}
