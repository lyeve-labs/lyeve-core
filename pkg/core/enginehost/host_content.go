package enginehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// UpsertContent mirrors a content write into the per-schema table, keeping the
// mirrored row's id aligned with the source record. Implements core.ContentWriter.
//
// A schema this tenant has not defined has no generated table to project into.
// That reaches the caller as core.ErrNotFound rather than the engine's internal
// sentinel, so a plugin can tell "nothing to mirror into" apart from a write
// that genuinely failed.
func (h *engineHost) UpsertContent(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) error {
	if h.contentStore == nil {
		return fmt.Errorf("content writer not wired")
	}
	err := h.contentStore.UpsertContent(ctx, schemaName, id, data)
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("%w: schema %q", core.ErrNotFound, schemaName)
	}
	return err
}

// DeleteContent mirrors a content delete into the per-schema table, treating a
// missing row as success: the mirror is a projection, so deleting an
// already-absent row is not an error. Implements core.ContentWriter.
func (h *engineHost) DeleteContent(ctx context.Context, schemaName string, id uuid.UUID) error {
	if h.contentStore == nil {
		return fmt.Errorf("content writer not wired")
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
// can import. Anything else is the store's own error, a database failure
// the caller answers as unavailable.
func contentWriterErr(err error, schemaName string) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("%w: schema %q", core.ErrNotFound, schemaName)
	case errors.Is(err, domain.ErrValidation):
		return fmt.Errorf("%w: %s", core.ErrValidation, err.Error())
	}
	return err
}

var (
	_ core.ContentWriter          = (*engineHost)(nil)
	_ core.ContentStatusProjector = (*engineHost)(nil)
)
