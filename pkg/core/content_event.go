package core

import (
	"context"
	"fmt"
	"time"
)

// SourceEngine is the Event.Source of a lifecycle event the engine's own
// content handlers publish.
const SourceEngine = "engine"

// ContentEvent builds the lifecycle event a content write publishes.
//
// Every writer of a generated content table publishes through
// PublishContentEvent, the engine's REST content handler and every plugin
// that writes content alike. Subscribers to after_create, after_update and
// after_delete read one shape on every schema, so the shape is built here and
// nowhere else.
//
// before is the row as it was and after the row as it is now. Data is the row
// the subscriber acts on: the new row for a create or an update, the removed
// row for a delete. OldData is set only on an update. The id is stamped into
// Data when the writer did not put it there, so a subscriber can read it from
// either field.
func ContentEvent(ctx context.Context, source string, action EventType, schema, id string, before, after map[string]any) Event {
	data, old := after, before
	if action == AfterDelete || action == BeforeDelete {
		data, old = before, nil
	}
	if action == AfterCreate || action == BeforeCreate {
		old = nil
	}
	return Event{
		Type:      action,
		Schema:    schema,
		Data:      withID(data, id),
		OldData:   old,
		RecordID:  id,
		TenantID:  TenantIDFromCtx(ctx),
		Timestamp: time.Now().UTC(),
		Source:    source,
	}
}

// PublishContentEvent publishes the event ContentEvent builds. A nil publisher
// publishes nothing, so a host that grants no hook capability costs the
// writer nothing. The write has already committed when this runs, so a caller
// logs the error rather than failing the request on it.
func PublishContentEvent(ctx context.Context, pub HookPublisher, source string, action EventType, schema, id string, before, after map[string]any) error {
	if pub == nil {
		return nil
	}
	if err := pub.Publish(ctx, ContentEvent(ctx, source, action, schema, id, before, after)); err != nil {
		return fmt.Errorf("publish %s for %s: %w", action, schema, err)
	}
	return nil
}

// withID returns data carrying the row id under "id". The caller's map is
// left as it is: a handler that goes on to mask and serialize it must not
// find a key the write did not produce.
func withID(data map[string]any, id string) map[string]any {
	if id == "" {
		return data
	}
	if _, ok := data["id"]; ok {
		return data
	}
	out := make(map[string]any, len(data)+1)
	for k, v := range data {
		out[k] = v
	}
	out["id"] = id
	return out
}
