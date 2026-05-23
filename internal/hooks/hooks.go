// Package hooks implements the plugin lifecycle hook bus. Publishers emit
// typed events (before and after create, update and delete, among others),
// and subscribers receive them synchronously, so plugins can observe (and
// veto) engine operations.
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/eventbus"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// EventType identifies when in the lifecycle a hook fires.
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
	// BeforeRequest fires at the start of every content handler, before the
	// request body is decoded. Returning an error aborts with HTTP 403.
	BeforeRequest EventType = "before_request"
	// AfterResponse fires after data is fetched and before it is serialized.
	// Hooks may delete keys from Event.Data to implement field masking.
	AfterResponse EventType = "after_response"
)

// Event carries the context of a lifecycle action.
type Event struct {
	Type   EventType
	Schema string
	// Method is the content operation: "list", "get", "create", "update", "delete".
	// Set on BeforeRequest and AfterResponse events.
	Method  string
	Data    map[string]any
	OldData map[string]any // nil for create events
	// TenantID is the tenant that originated this event.
	// Set by HookBus.Publish when the source event carries a tenant.
	// Subscribers that fan out across tenants (e.g. webhook dispatch)
	// use this as the authoritative tenant identity.
	TenantID string
	// RecordID is the id of the content row the event is about, when the
	// publisher knows it.
	RecordID string
	// Source names the publisher. A subscriber that also writes content
	// reads it to skip the events its own writes produce.
	Source string
}

// Hook is a function that runs on a lifecycle event.
// Returning an error aborts the operation (maps to 422).
type Hook func(ctx context.Context, event Event) error

// hookEntry represents a dynamically registered hook in the byID index.
type hookEntry struct {
	key   string
	idx   int
	owner string // random token proving ownership. Required for Unregister
}

// SystemHandler is a handler for named system events (e.g. "license.changed").
// System events are not schema-scoped and carry arbitrary key-value data.
type SystemHandler func(ctx context.Context, data map[string]any) error

// systemEntry tracks a system event handler by token. seq preserves the order
// the handlers were registered in: the map they live in is keyed by a random
// token, so ranging it hands them back in a different order almost every time.
type systemEntry struct {
	event string
	fn    SystemHandler
	seq   uint64
}

// Registry holds all registered hooks keyed by "{schema}.{event_type}".
// Use "*" as the schema name to register a hook that fires for every schema.
type Registry struct {
	mu          sync.RWMutex
	hooks       map[string][]Hook
	byID        map[string]hookEntry // publicID -> entry: for Unregister
	sysMu       sync.RWMutex
	sysHandlers map[string]map[string]systemEntry // event -> token -> entry
	sysSeq      uint64                            // registration counter, for ordering
}

// NewRegistry returns an empty hook registry.
func NewRegistry() *Registry {
	return &Registry{
		hooks: make(map[string][]Hook),
		byID:  make(map[string]hookEntry),
	}
}

// Register adds a hook for a specific schema and event type.
// Use "*" for schema to match all schemas.
func (r *Registry) Register(schema string, eventType EventType, fn Hook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key(schema, eventType)
	r.hooks[k] = append(r.hooks[k], fn)
}

// RegisterDynamic adds a hook and returns a compound registration token
// ("publicID.ownerSecret") that must be passed to Unregister later.
// Intended for per-request lifecycle hooks (e.g., SSE streams).
// The caller MUST retain the full token. The owner component is required
// for Unregister to prove ownership of the registration.
// Format: base32publicID.hexownerSecret
func (r *Registry) RegisterDynamic(schema string, eventType EventType, fn Hook) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := uuid.New().String()
	owner := uuid.New().String()
	k := key(schema, eventType)
	idx := len(r.hooks[k])
	r.hooks[k] = append(r.hooks[k], fn)
	r.byID[id] = hookEntry{key: k, idx: idx, owner: owner}
	return id + "." + owner
}

// Unregister removes a hook previously registered via RegisterDynamic.
// token must be the full compound value returned by RegisterDynamic
// ("publicID.ownerSecret"). If the owner component doesn't match the
// stored secret, the call is silently ignored (not an error).
// It swap-removes the hook from its slice (compacting rather than
// replacing with a no-op) to prevent unbounded growth from repeated
// register/unregister cycles (e.g., per-request SSE hooks).
func (r *Registry) Unregister(token string) {
	id, owner, ok := parseCompoundToken(token)
	if !ok {
		return // malformed token: silently ignore
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.byID[id]
	if !ok {
		return
	}
	if info.owner != owner {
		return // ownership mismatch: silently ignore
	}
	delete(r.byID, id)
	hooks := r.hooks[info.key]
	lastIdx := len(hooks) - 1
	if info.idx < lastIdx {
		// Swap-remove: move the last element into the removed slot.
		hooks[info.idx] = hooks[lastIdx]
		// The displaced hook's byID entry now points to a stale index.
		// Scan and fix it.
		for pubID, entry := range r.byID {
			if entry.key == info.key && entry.idx == lastIdx {
				entry.idx = info.idx
				r.byID[pubID] = entry
				break
			}
		}
	}
	hooks = hooks[:lastIdx]
	if len(hooks) == 0 {
		delete(r.hooks, info.key)
	} else {
		r.hooks[info.key] = hooks
	}
}

// RegisterSystem registers a handler for a named system event.
// Returns a token that must be passed to UnregisterSystem to remove the handler.
func (r *Registry) RegisterSystem(event string, fn SystemHandler) string {
	r.sysMu.Lock()
	defer r.sysMu.Unlock()
	if r.sysHandlers == nil {
		r.sysHandlers = make(map[string]map[string]systemEntry)
	}
	token := uuid.New().String()
	if r.sysHandlers[event] == nil {
		r.sysHandlers[event] = make(map[string]systemEntry)
	}
	r.sysSeq++
	r.sysHandlers[event][token] = systemEntry{event: event, fn: fn, seq: r.sysSeq}
	return token
}

// UnregisterSystem removes a system event handler previously registered via RegisterSystem.
func (r *Registry) UnregisterSystem(token string) {
	r.sysMu.Lock()
	defer r.sysMu.Unlock()
	for event, handlers := range r.sysHandlers {
		if _, ok := handlers[token]; ok {
			delete(handlers, token)
			if len(handlers) == 0 {
				delete(r.sysHandlers, event)
			}
			return // each token is unique across all events
		}
	}
}

// RunSystem fires all handlers registered for the named system event.
// Returns the first error encountered. Remaining handlers are not executed.
func (r *Registry) RunSystem(ctx context.Context, event string, data map[string]any) error {
	r.sysMu.RLock()
	handlers := r.sysHandlers[event]
	// Copy handlers while holding the lock, then restore registration order.
	// The runtime registers its own license.changed handler after every plugin
	// has started and depends on running last, because it re-reads the route
	// tables the plugins' handlers have just rebuilt. Ranging the map gives it
	// no such guarantee: it would run first about a quarter of the time, against
	// the tables the license change is meant to replace.
	entries := make([]systemEntry, 0, len(handlers))
	for _, entry := range handlers {
		entries = append(entries, entry)
	}
	r.sysMu.RUnlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })

	all := make([]SystemHandler, 0, len(entries))
	for _, entry := range entries {
		all = append(all, entry.fn)
	}

	for _, fn := range all {
		if err := fn(ctx, data); err != nil {
			return fmt.Errorf("system event %s: %w", event, err)
		}
	}
	return nil
}

// parseCompoundToken splits "publicID.ownerSecret" into its components.
// Returns false if the token doesn't contain exactly one dot or either
// component is empty.
func parseCompoundToken(token string) (id, owner string, ok bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	if parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// Run executes all hooks registered for the event in order:
// wildcard ("*") hooks first, then schema-specific hooks.
// Stops on first error.
func (r *Registry) Run(ctx context.Context, event Event) error {
	// The emitters build the event from what the handler has to hand, and the
	// tenant is not in it: it is on the context, put there by the tenancy
	// middleware. Filling it here covers every emitter at once, which is what
	// the field's own contract says should happen, so a subscriber that fans
	// out per tenant always has one.
	if event.TenantID == "" {
		event.TenantID = core.TenantIDFromCtx(ctx)
	}

	r.mu.RLock()
	wildcardFns := r.hooks[key("*", event.Type)]
	specificFns := r.hooks[key(event.Schema, event.Type)]
	// Copy slices while holding the lock to avoid data races.
	all := make([]Hook, 0, len(wildcardFns)+len(specificFns))
	all = append(all, wildcardFns...)
	all = append(all, specificFns...)
	r.mu.RUnlock()

	for _, fn := range all {
		if err := fn(ctx, event); err != nil {
			return fmt.Errorf("hook %s/%s: %w", event.Schema, event.Type, err)
		}
	}
	return nil
}

// Publish runs a public-surface event through the registry, so the registry
// itself is the core.HookPublisher the engine's own handlers publish with.
// The plugin host adapter delegates here for the same reason: one mapping
// from the public event to the internal one, whoever publishes.
func (r *Registry) Publish(ctx context.Context, event core.Event) error {
	return r.Run(ctx, Event{
		Type:     EventType(event.Type),
		Schema:   event.Schema,
		Data:     event.Data,
		OldData:  event.OldData,
		TenantID: event.TenantID,
		RecordID: event.RecordID,
		Source:   event.Source,
	})
}

var _ core.HookPublisher = (*Registry)(nil)

func key(schema string, eventType EventType) string {
	return schema + "." + string(eventType)
}

// BusHook: publish events to a message bus

// busPayload is the JSON envelope published to the message bus.
type busPayload struct {
	Schema        string         `json:"schema"`
	EventType     EventType      `json:"event_type"`
	TenantID      string         `json:"tenant_id,omitempty"`
	Data          map[string]any `json:"data,omitempty"`
	OldData       map[string]any `json:"old_data,omitempty"`
	MessageID     string         `json:"message_id"`
	CorrelationID string         `json:"correlation_id"`
	Timestamp     string         `json:"timestamp"`
	ProducerID    string         `json:"producer_id"`
}

// CorrelationIDKey is a context key for extracting a correlation ID from
// the context chain. When present, its value is used as CorrelationID on
// outgoing bus messages. Otherwise the MessageID is reused.
// Intended to be set by request-ID middleware or upstream callers.
type CorrelationIDKey struct{}

// NewBusHook returns an After* hook that serializes and publishes the event to
// publisher. Topic follows the convention "cms.tenant.{tenant_id}.{schema}.{event_type}".
// When no tenant is present in the context, the topic falls back to
// "cms.{schema}.{event_type}" for single-tenant compatibility and a warning
// is logged. The payload still includes tenant_id (empty string).
//
// Errors from the publisher are logged but do not abort the hook chain: the
// content write already succeeded at this point.
func NewBusHook(publisher eventbus.Publisher) Hook {
	producerID := uuid.New().String()
	return func(ctx context.Context, e Event) error {
		tenantID := tenant.ID(ctx)
		now := time.Now().UTC()
		messageID := uuid.New().String()
		correlationID := correlationIDFromContext(ctx, messageID)
		p := busPayload{
			Schema:        e.Schema,
			EventType:     e.Type,
			TenantID:      tenantID,
			Data:          e.Data,
			OldData:       e.OldData,
			MessageID:     messageID,
			CorrelationID: correlationID,
			Timestamp:     now.Format(time.RFC3339Nano),
			ProducerID:    producerID,
		}
		payload, err := json.Marshal(p)
		if err != nil {
			slog.ErrorContext(ctx, "bus hook: marshal failed", "schema", e.Schema, "event", e.Type, "err", err)
			return nil // don't abort. Serialization failure is a programming error
		}
		var topic string
		if tenantID != "" {
			topic = fmt.Sprintf("cms.tenant.%s.%s.%s", tenantID, e.Schema, string(e.Type))
		} else {
			topic = fmt.Sprintf("cms.%s.%s", e.Schema, string(e.Type))
			slog.WarnContext(ctx, "bus hook: publishing without tenant",
				"schema", e.Schema,
				"event", e.Type,
				"topic", topic,
			)
		}
		if err := publisher.Publish(ctx, topic, payload); err != nil {
			slog.ErrorContext(ctx, "bus hook: publish failed", "topic", topic, "err", err)
		}
		return nil // fire-and-forget. Do not abort
	}
}

// correlationIDFromContext extracts a correlation ID from the context.
// If no CorrelationIDKey value is present, fallback is returned (typically
// the MessageID so every message is at least self-correlated).
func correlationIDFromContext(ctx context.Context, fallback string) string {
	if v, ok := ctx.Value(CorrelationIDKey{}).(string); ok && v != "" {
		return v
	}
	return fallback
}
