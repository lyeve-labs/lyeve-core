package enginehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// UpsertContent mirrors a content write into the per-schema table, keeping the
// mirrored row's id aligned with the source record. Implements core.ContentWriter.
//
// A schema this tenant has not defined has no generated table to project into.
// That reaches the caller as core.ErrNotFound rather than the engine's internal
// sentinel, so a plugin can tell "nothing to mirror into" apart from a write
// that genuinely failed. A value the store refuses, such as a _status no read
// filter matches, is core.ErrValidation, so the plugin answers it as the
// caller's mistake and not as an outage the caller would retry. A soft-deleted
// row under the id is core.ErrConflict, and the row stays deleted.
func (h *engineHost) UpsertContent(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) error {
	return h.UpsertContentAs(ctx, "", schemaName, id, data)
}

// UpsertContentAs is UpsertContent on behalf of the plugin named plugin.
// Implements core.PluginContentWriter.
//
// Before it inserts a row, whether the row was missing or the update fell
// back to an insert, it publishes before_create with the caller's ctx and
// Plugin set to plugin, and a refusal comes back wrapping
// core.ErrCreateRefused with nothing written. Before it updates a row the
// tenant holds, it publishes before_update the same way, and a refusal comes
// back wrapping core.ErrUpdateRefused. So every writer that reaches the
// table through the host meets the subscribers a write through the engine's
// own routes meets. The content plugin's update asks nobody, as
// asksBeforeEdit says.
func (h *engineHost) UpsertContentAs(ctx context.Context, plugin, schemaName string, id uuid.UUID, data map[string]any) error {
	if h.contentStore == nil {
		return fmt.Errorf("content writer not wired")
	}
	before := func(ctx context.Context) error {
		if h.registry == nil {
			return nil
		}
		ev := core.ContentEvent(ctx, core.SourceEngine, core.BeforeCreate, schemaName, id.String(), nil, data)
		ev.Plugin = plugin
		if err := h.HookPublisher().Publish(ctx, ev); err != nil {
			return refusedBy(core.ErrCreateRefused, schemaName, err)
		}
		return nil
	}
	var beforeUpdate func(context.Context, map[string]any) error
	if h.asksBeforeEdit(plugin) {
		beforeUpdate = func(ctx context.Context, old map[string]any) error {
			return h.askBeforeEdit(ctx, plugin, core.BeforeUpdate, schemaName, id, old, data)
		}
	}
	if err := h.contentStore.UpsertContentChecked(ctx, schemaName, id, data, beforeUpdate, before); err != nil {
		return contentWriterErr(err, schemaName)
	}
	return nil
}

// asksBeforeEdit reports whether an update or a delete by plugin asks the
// before_update or before_delete subscribers. The content plugin's writes
// project an entry whose own update or delete asked them already, and a
// projection must never be refused once the entry has changed.
func (h *engineHost) asksBeforeEdit(plugin string) bool {
	return h.registry != nil && plugin != core.ContentPluginName
}

// askBeforeEdit publishes before_update or before_delete for the row id with
// Plugin set to plugin, and answers a refusal wrapping core.ErrUpdateRefused
// or core.ErrDeleteRefused.
func (h *engineHost) askBeforeEdit(ctx context.Context, plugin string, action core.EventType, schemaName string, id uuid.UUID, old, data map[string]any) error {
	ev := core.ContentEvent(ctx, core.SourceEngine, action, schemaName, id.String(), old, data)
	ev.Plugin = plugin
	if err := h.HookPublisher().Publish(ctx, ev); err != nil {
		refused := core.ErrUpdateRefused
		if action == core.BeforeDelete {
			refused = core.ErrDeleteRefused
		}
		return refusedBy(refused, schemaName, err)
	}
	return nil
}

// refusedBy is a before subscriber's error as the writer answers it: a
// refusal wrapping refused, or, when the subscriber could not read a store it
// needs, the core.ErrServiceUnavailable it returned, so the caller answers an
// outage and not a refusal.
func refusedBy(refused error, schemaName string, err error) error {
	if errors.Is(err, core.ErrServiceUnavailable) {
		return fmt.Errorf("ask the subscribers of %s: %w", schemaName, err)
	}
	return fmt.Errorf("%w: %s: %w", refused, schemaName, err)
}

// ProjectContentAs writes as UpsertContent does and publishes no
// before_create, for the content plugin alone, which asked the subscribers
// when it created the entry. Any other plugin is core.ErrCapDenied, and
// nothing is written. Implements core.PluginContentWriter.
func (h *engineHost) ProjectContentAs(ctx context.Context, plugin, schemaName string, id uuid.UUID, data map[string]any) error {
	if plugin != core.ContentPluginName {
		return fmt.Errorf("project content: %w", core.ErrCapDenied)
	}
	if h.contentStore == nil {
		return fmt.Errorf("content writer not wired")
	}
	if err := h.contentStore.UpsertContent(ctx, schemaName, id, data); err != nil {
		return contentWriterErr(err, schemaName)
	}
	return nil
}

// FlushContentCache drops the content read caches after a plugin wrote a
// content table with its own SQL. Implements core.ContentCacheFlusher.
func (h *engineHost) FlushContentCache(ctx context.Context, schemaName string) {
	if h.contentStore != nil {
		h.contentStore.FlushWritten(ctx, schemaName)
	}
}

// UpdateContent writes data to an existing row of the per-schema table and
// never inserts one. Implements core.ContentUpdater.
//
// The store's update matches the row by id and tenant, and skips a
// soft-deleted row, so a row that is gone matches nothing. That miss reaches
// the caller as core.ErrNotFound, the answer a read of the row would give.
func (h *engineHost) UpdateContent(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) error {
	return h.UpdateContentAs(ctx, "", schemaName, id, data)
}

// UpdateContentAs is UpdateContent on behalf of the plugin named plugin.
// Implements core.PluginContentEditor. It reads the row and publishes
// before_update with the caller's ctx and Plugin set to plugin before it
// writes, and a refusal comes back wrapping core.ErrUpdateRefused with
// nothing written. The content plugin's update asks nobody, as
// asksBeforeEdit says.
func (h *engineHost) UpdateContentAs(ctx context.Context, plugin, schemaName string, id uuid.UUID, data map[string]any) error {
	if h.contentStore == nil {
		return fmt.Errorf("content writer not wired")
	}
	if h.asksBeforeEdit(plugin) {
		existing, err := h.contentStore.GetByID(ctx, schemaName, id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return fmt.Errorf("%w: %s row %s", core.ErrNotFound, schemaName, id)
			}
			return contentWriterErr(err, schemaName)
		}
		if err := h.askBeforeEdit(ctx, plugin, core.BeforeUpdate, schemaName, id, existing.Data, data); err != nil {
			return err
		}
	}
	if _, err := h.contentStore.Update(ctx, schemaName, id, data, time.Time{}); err != nil {
		if errors.Is(err, db.ErrContentConflict) {
			return fmt.Errorf("%w: %s row %s", core.ErrNotFound, schemaName, id)
		}
		return contentWriterErr(err, schemaName)
	}
	return nil
}

// DeleteContent mirrors a content delete into the per-schema table, treating a
// missing row as success: the mirror is a projection, so deleting an
// already-absent row is not an error. Implements core.ContentWriter.
func (h *engineHost) DeleteContent(ctx context.Context, schemaName string, id uuid.UUID) error {
	return h.DeleteContentAs(ctx, "", schemaName, id)
}

// DeleteContentAs is DeleteContent on behalf of the plugin named plugin.
// Implements core.PluginContentEditor. It reads the row and publishes
// before_delete with the caller's ctx and Plugin set to plugin before it
// deletes, and a refusal comes back wrapping core.ErrDeleteRefused with the
// row kept. A row that is already gone asks nobody. The content plugin's
// delete asks nobody, as asksBeforeEdit says.
func (h *engineHost) DeleteContentAs(ctx context.Context, plugin, schemaName string, id uuid.UUID) error {
	if h.contentStore == nil {
		return fmt.Errorf("content writer not wired")
	}
	if h.asksBeforeEdit(plugin) {
		existing, err := h.contentStore.GetByID(ctx, schemaName, id)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return contentWriterErr(err, schemaName)
		}
		if err := h.askBeforeEdit(ctx, plugin, core.BeforeDelete, schemaName, id, existing.Data, nil); err != nil {
			return err
		}
	}
	if err := h.contentStore.Delete(ctx, schemaName, id); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return nil
}

// SetContentStatus moves a row of the per-schema table between draft,
// published and archived through the store the engine's own publish route
// writes, so the row leaves the read caches the same way. Implements
// core.ContentWriter.
//
// A row the tenant does not have, or a schema it never defined, is
// core.ErrNotFound, and a status the column cannot hold is
// core.ErrValidation, so a plugin answers each the way it answers its own
// store. The row as it reads after the write goes out as the after_update
// lifecycle event, the shape every other content writer publishes, so a
// subscriber that reindexes or notifies on an update sees a publish the way
// it sees an edit. The write has committed by then, so a subscriber's
// failure is logged and the caller still sees success.
func (h *engineHost) SetContentStatus(ctx context.Context, schemaName string, id uuid.UUID, status string) error {
	if h.contentStore == nil {
		return fmt.Errorf("content writer not wired")
	}
	before, err := h.contentStore.GetByID(ctx, schemaName, id)
	if err != nil {
		return contentWriterErr(err, schemaName)
	}
	if err := h.contentStore.SetStatus(ctx, schemaName, id, status); err != nil {
		return contentWriterErr(err, schemaName)
	}
	after, err := h.contentStore.GetByID(ctx, schemaName, id)
	if err != nil {
		return contentWriterErr(err, schemaName)
	}
	var pub core.HookPublisher
	if h.registry != nil {
		pub = h.HookPublisher()
	}
	if err := core.PublishContentEvent(ctx, pub, core.SourceEngine, core.AfterUpdate, schemaName, id.String(), before.Data, after.Data); err != nil {
		slog.WarnContext(ctx, "content: after-write hook failed", "schema", schemaName, "event", core.AfterUpdate, "err", err)
	}
	return nil
}

// ProjectContentStatus writes the status SetContentStatus writes, through
// the same store, and publishes nothing. The caller announces the change.
// Implements core.ContentStatusProjector.
func (h *engineHost) ProjectContentStatus(ctx context.Context, schemaName string, id uuid.UUID, status string) error {
	if h.contentStore == nil {
		return fmt.Errorf("content writer not wired")
	}
	if err := h.contentStore.SetStatus(ctx, schemaName, id, status); err != nil {
		return contentWriterErr(err, schemaName)
	}
	return nil
}

// contentWriterErr translates the store's sentinels into the ones a plugin
// can import. A validation error keeps the store's chain, so a refusal that
// names its field, such as a *core.RelationTargetError, still reads with
// errors.As. Anything else is the store's own error, a database failure the
// caller answers as unavailable.
func contentWriterErr(err error, schemaName string) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("%w: schema %q", core.ErrNotFound, schemaName)
	case errors.Is(err, domain.ErrValidation):
		return fmt.Errorf("%w: %w", core.ErrValidation, err)
	case errors.Is(err, domain.ErrConflict):
		return fmt.Errorf("%w: %w", core.ErrConflict, err)
	}
	return err
}

// ContentPage reads one page of a schema's generated table for the tenant on
// ctx, drafts included and soft-deleted rows left out. Implements
// core.ContentReader.
func (h *engineHost) ContentPage(ctx context.Context, schemaName string, after uuid.UUID, limit int) ([]map[string]any, error) {
	if h.contentStore == nil {
		return nil, fmt.Errorf("content reader not wired")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	cursor := ""
	if after != uuid.Nil {
		cursor = after.String()
	}
	items, err := h.contentStore.ListCursor(ctx, schemaName, cursor, limit, true)
	if err != nil {
		return nil, contentWriterErr(err, schemaName)
	}
	rows := make([]map[string]any, 0, len(items))
	for _, c := range items {
		row := make(map[string]any, len(c.Data)+3)
		for k, v := range c.Data {
			row[k] = v
		}
		row["id"] = c.ID.String()
		row["created_at"] = c.CreatedAt
		row["updated_at"] = c.UpdatedAt
		rows = append(rows, row)
	}
	return rows, nil
}

var (
	_ core.ContentWriter          = (*engineHost)(nil)
	_ core.PluginContentWriter    = (*engineHost)(nil)
	_ core.PluginContentEditor    = (*engineHost)(nil)
	_ core.ContentUpdater         = (*engineHost)(nil)
	_ core.ContentStatusProjector = (*engineHost)(nil)
	_ core.ContentReader          = (*engineHost)(nil)
)
