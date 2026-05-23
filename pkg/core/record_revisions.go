package core

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// RecordRevision is one historical snapshot of a row in a schema's generated
// table, captured before the update that replaced it. Data holds the whole
// row as it stood at capture time, so a restore writes it back unchanged.
//
// This is the schema-driven content API's history, keyed by a schema name and
// a record id. The editorial content store keeps its own history per entry,
// numbered and diffable, which is a different record of a different thing.
type RecordRevision struct {
	ID         uuid.UUID      `json:"id"`
	SchemaName string         `json:"schema_name"`
	RecordID   uuid.UUID      `json:"record_id"`
	Data       map[string]any `json:"data"`
	CreatedBy  *uuid.UUID     `json:"created_by,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// RecordRevisionStore keeps the revision history behind
// /api/v1/content/{schema}/{id}/revisions. The engine writes a snapshot
// before every update and reads one back to restore it, and the plugin that
// owns the rows decides where they live.
//
// Every call is scoped to the tenant on ctx.
type RecordRevisionStore interface {
	// SaveRecordRevision snapshots data as it stands before an update.
	// actor is the user the snapshot is attributed to, or nil for a write
	// by an API key or an unauthenticated caller.
	SaveRecordRevision(ctx context.Context, schemaName string, recordID uuid.UUID, data map[string]any, actor *uuid.UUID) error

	// ListRecordRevisions returns a record's snapshots, newest first.
	ListRecordRevisions(ctx context.Context, schemaName string, recordID uuid.UUID) ([]RecordRevision, error)

	// GetRecordRevision returns one snapshot by its id. A snapshot the
	// tenant does not hold is an error wrapping ErrRecordRevisionNotFound.
	GetRecordRevision(ctx context.Context, revisionID uuid.UUID) (RecordRevision, error)
}

// ErrRecordRevisionNotFound is what a store returns for a revision id the
// tenant holds no row for. The handler answers it as 404.
var ErrRecordRevisionNotFound = errors.New("record revision not found")

// RecordRevisionStoreRegistrar is implemented by the engine host. The plugin
// that owns the rows calls it when it starts and again with nil when it
// stops, so the registration follows the plugin's lifecycle, not the
// process.
type RecordRevisionStoreRegistrar interface {
	RegisterRecordRevisionStore(s RecordRevisionStore)
}

// RecordRevisionStoreProvider is implemented by the engine host and
// forwarded by ScopedHost. The content handlers read it per request, never
// at Start, because the owning plugin may start after the router is built.
//
// Nil means no plugin keeps revisions. A build in that state answers the two
// revision routes 503 rather than an empty list, because an empty history for
// a record that has been edited reads as data loss. An update still succeeds
// and simply records nothing.
type RecordRevisionStoreProvider interface {
	RecordRevisionStore() RecordRevisionStore
}
