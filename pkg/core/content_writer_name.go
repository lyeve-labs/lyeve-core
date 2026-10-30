package core

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ContentPluginName is the name the content plugin registers under. It is
// the one plugin a host lets project an entry into its content type's table
// without asking the before_create subscribers, because the entry's create
// asked them already.
const ContentPluginName = "content"

// ErrCreateRefused is a create a before_create subscriber refused. Nothing
// was written. It wraps ErrValidation, so a caller that maps errors by
// sentinel reads it as the caller's mistake and never as an outage. A route
// answers it 422, the answer the engine's own content routes give a refused
// create.
var ErrCreateRefused = fmt.Errorf("create refused by a before_create hook: %w", ErrValidation)

// PublishBeforeCreate asks the before_create subscribers of schema whether
// the entry id may be created with data, and answers an error wrapping
// ErrCreateRefused when one refuses. A writer calls it once per entry it
// creates, before its first statement, and writes nothing when it refuses. A
// nil publisher asks nobody.
//
// The event's Plugin is the caller's name: the publisher a plugin is handed
// writes it, so a subscriber can tell which plugin asked.
func PublishBeforeCreate(ctx context.Context, pub HookPublisher, source, schema string, id uuid.UUID, data map[string]any) error {
	if pub == nil {
		return nil
	}
	rid := ""
	if id != uuid.Nil {
		rid = id.String()
	}
	if err := pub.Publish(ctx, ContentEvent(ctx, source, BeforeCreate, schema, rid, nil, data)); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCreateRefused, schema, err)
	}
	return nil
}

// ContentProjector writes an entry's row into its content type's table as
// UpsertContent does, and asks no before_create subscriber when the row is
// new. It is for the content plugin alone, which keeps the entries and
// projects each into its table: the entry's create asked the subscribers
// once, and a later projection of the same entry, such as one that writes
// back a row that went missing, must not ask again and must never be
// refused. A host answers ErrCapDenied, and writes nothing, for any other
// caller.
type ContentProjector interface {
	ProjectContent(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) error
}

// PluginContentWriter is the content write a host makes on behalf of a named
// plugin. The scoped host a plugin is handed calls it on the host it wraps
// with the plugin's registered name, and ignores the name its own caller
// passes, so the layer next to the engine names the caller last and a plugin
// cannot write under another's name.
//
// UpsertContentAs is ContentWriter.UpsertContent, and publishes before_create
// with Plugin set to plugin before it inserts a row. ProjectContentAs is
// ContentProjector.ProjectContent, refused with ErrCapDenied unless plugin is
// ContentPluginName.
type PluginContentWriter interface {
	UpsertContentAs(ctx context.Context, plugin, schemaName string, id uuid.UUID, data map[string]any) error
	ProjectContentAs(ctx context.Context, plugin, schemaName string, id uuid.UUID, data map[string]any) error
}
