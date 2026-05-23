package core

import (
	"context"

	"github.com/google/uuid"
)

// ContentEntryWriter writes entries into the editorial content store: the
// store the admin's content screens read, which keeps a revision per write
// and projects every write into the schema's generated table. The plugin
// that owns that store registers one. A plugin that creates content on a
// person's behalf, an import for one, writes through it, so what it writes
// is what an editor sees, what an export reads, and what the public routes
// serve. Writing the generated table directly reaches only the last of the
// three.
//
// Every call is scoped to the tenant on ctx and refuses a context without
// one with ErrTenantRequired.
type ContentEntryWriter interface {
	// SaveContentEntry writes one entry. With MatchField empty it always
	// creates. With MatchField set it first looks for an entry of the same
	// schema whose field holds the same value, and OnMatch decides what
	// happens to one it finds.
	//
	// A row the store refuses is an error wrapping ErrValidation (a field
	// the schema rejects, a slug with characters a slug cannot hold, an
	// unknown status) or ErrConflict (a slug another entry of the tenant
	// already holds). The message says which field and why, in words a
	// person can act on, and carries no driver text. Anything else is a
	// store failure.
	SaveContentEntry(ctx context.Context, in ContentEntryInput) (ContentEntrySaved, error)

	// DeleteContentEntry removes an entry the tenant holds, with the
	// projection and the lifecycle event a delete from the admin carries.
	// An entry the tenant does not hold is ErrNotFound.
	DeleteContentEntry(ctx context.Context, id uuid.UUID) error
}

// ContentEntryInput is one entry to write.
type ContentEntryInput struct {
	// Schema is the content type the entry belongs to. It must be one the
	// tenant has defined.
	Schema string

	// Slug identifies the entry in URLs and is unique within the tenant.
	// Empty derives one from the title. A derived slug that is taken gets a
	// short suffix rather than failing the write.
	Slug string

	// Title is the entry's display title. Empty takes the fields' "title"
	// value, then the slug.
	Title string

	// Status is draft, published or archived. Empty creates a draft and
	// leaves an existing entry's status as it is.
	Status string

	// Fields are the entry's values, keyed by the schema's field names.
	Fields map[string]any

	// MatchField names the field that identifies an existing entry: "slug",
	// or one of the schema's fields. Empty always creates.
	MatchField string

	// OnMatch is what happens to an existing entry MatchField finds.
	OnMatch ContentMatchAction

	// Actor is the user the write is attributed to in the entry's history.
	// uuid.Nil records no user.
	Actor uuid.UUID
}

// ContentMatchAction decides what SaveContentEntry does with an existing
// entry that holds the same MatchField value.
type ContentMatchAction string

const (
	// ContentMatchSkip leaves the existing entry alone and reports the
	// write as skipped. It is the default.
	ContentMatchSkip ContentMatchAction = "skip"
	// ContentMatchUpdate overwrites the existing entry's fields with the
	// ones given, keeping fields the input does not name.
	ContentMatchUpdate ContentMatchAction = "update"
)

// ContentEntrySaved reports what SaveContentEntry did.
type ContentEntrySaved struct {
	// ID is the entry written, or the existing entry a skip left alone.
	ID uuid.UUID
	// Outcome is ContentEntryCreated, ContentEntryUpdated or
	// ContentEntrySkipped.
	Outcome string
}

// The outcomes SaveContentEntry reports.
const (
	ContentEntryCreated = "created"
	ContentEntryUpdated = "updated"
	ContentEntrySkipped = "skipped"
)

// ContentEntryWriterRegistrar is implemented by the engine host. The plugin
// that owns the editorial store calls it when it activates and again with
// nil when it stops, so the registration follows the plugin's lifecycle,
// not the process.
type ContentEntryWriterRegistrar interface {
	RegisterContentEntryWriter(w ContentEntryWriter)
}

// ContentEntryWriterProvider is implemented by the engine host and forwarded
// by ScopedHost. A consumer asks it per job, never at Start, because the
// owning plugin may start after it. Nil means no plugin has registered a
// writer.
type ContentEntryWriterProvider interface {
	ContentEntryWriter() ContentEntryWriter
}
