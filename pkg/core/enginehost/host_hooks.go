package enginehost

import (
	"context"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// hookBusAdapter wraps *hooks.Registry to implement core.HookBus.
type hookBusAdapter struct {
	registry *hooks.Registry
}

// Subscribe registers handler for the given schema and event type.
// The returned Subscription must be Unsubscribed when the caller is done to
// avoid leaking no-op entries in the registry.
func (h *hookBusAdapter) Subscribe(schema string, event core.EventType, handler core.EventHandler) core.Subscription {
	// Map core.EventType -> hooks.EventType (identical string values).
	internalEvent := hooks.EventType(event)

	// Wrap the public EventHandler into the internal hooks.Hook signature.
	fn := func(ctx context.Context, e hooks.Event) error {
		return handler(ctx, core.Event{
			Type:     core.EventType(e.Type),
			Schema:   e.Schema,
			Data:     e.Data,
			OldData:  e.OldData,
			TenantID: e.TenantID,
			RecordID: e.RecordID,
			Source:   e.Source,
		})
	}

	id := h.registry.RegisterDynamic(schema, internalEvent, fn)
	return &subscription{registry: h.registry, id: id}
}

// Publish fires event through the internal registry so all registered hooks
// (including those registered by the engine itself) receive it.
// When the event Source field is empty, a warning is logged: callers should
// set Source to identify themselves.
func (h *hookBusAdapter) Publish(ctx context.Context, event core.Event) error {
	// Log a warning for anonymous Publish calls (no Source set).
	// Trusted callers set Source explicitly.
	if event.Source == "" {
		slog.WarnContext(ctx, "hookbus: anonymous Publish call without Source",
			"type", event.Type,
			"schema", event.Schema,
			"tenant", event.TenantID,
		)
	}
	// Stamp the tenant from the event onto the context so subscribers
	// that read TenantIDFromCtx get the authoritative tenant regardless
	// of whether the caller pre-stamped ctx.
	if event.TenantID != "" {
		ctx = core.WithTenantID(ctx, event.TenantID)
	}
	return h.registry.Publish(ctx, event)
}

// On registers a handler for a named system event (e.g. "license.changed").
func (h *hookBusAdapter) On(event string, handler core.SystemEventHandler) core.Subscription {
	fn := func(ctx context.Context, data map[string]any) error {
		return handler(ctx, core.HookEvent{Name: event, Data: data})
	}
	token := h.registry.RegisterSystem(event, fn)
	return &sysSubscription{registry: h.registry, token: token}
}

// PublishSystem fires a system event to all registered handlers.
func (h *hookBusAdapter) PublishSystem(ctx context.Context, event core.HookEvent) error {
	return h.registry.RunSystem(ctx, event.Name, event.Data)
}

// sysSubscription wraps a system event handler token.
type sysSubscription struct {
	registry *hooks.Registry
	token    string
}

func (s *sysSubscription) Unsubscribe() {
	s.registry.UnregisterSystem(s.token)
}

// subscription: implements Subscription

type subscription struct {
	registry *hooks.Registry
	id       string
}

func (s *subscription) Unsubscribe() {
	s.registry.Unregister(s.id)
}
