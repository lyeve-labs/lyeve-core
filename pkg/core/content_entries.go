package core

import (
	"context"
	"time"

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
	// SaveContentEntry writes one entry. With ID set it updates exactly
	// that entry and never creates. Otherwise, with MatchField empty it
	// always creates, and with MatchField set it first looks for an entry
	// of the same schema whose field holds the same value, and OnMatch
	// decides what happens to one it finds.
	//
	// A row the store refuses is an error wrapping ErrValidation (a field
	// the schema rejects, a slug with characters a slug cannot hold, an
	// unknown status) or ErrConflict (a slug another entry of the tenant
	// already holds, or an entry no longer at ExpectedRevision). An ID the
	// tenant does not hold under Schema is ErrNotFound. The message says
	// which field and why, in words a person can act on, and carries no
	// driver text. Anything else is a store failure.
	SaveContentEntry(ctx context.Context, in ContentEntryInput) (ContentEntrySaved, error)

	// DeleteContentEntry removes an entry the tenant holds, with the
	// projection and the lifecycle event a delete from the admin carries.
	// An entry the tenant does not hold is ErrNotFound.
	DeleteContentEntry(ctx context.Context, id uuid.UUID) error
}

// ContentEntryInput is one entry to write.
type ContentEntryInput struct {
	// ID names the entry to update. uuid.Nil creates or matches as
	// MatchField says. A set ID updates that entry of Schema in the tenant
	// on ctx and never creates. An entry another tenant holds, one filed
	// under another schema and a deleted one are all ErrNotFound. ID and
	// MatchField together are ErrValidation.
	ID uuid.UUID

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

	// ExpectedRevision is the revision the caller's edit started from. 0
	// checks nothing, and the write lands over whatever is stored. Above 0
	// it needs ID, and the store compares it with the entry's revision
	// under the same lock the write takes, so of two writes from one
	// revision exactly one lands and the other is ErrConflict.
	ExpectedRevision int

	// Note labels an update in the entry's history. Empty keeps the label
	// the store writes for this kind of write. A create keeps the store's
	// label for a first revision.
	Note string
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
	// Revision is the entry's revision after the write, which a caller
	// passes as ExpectedRevision on its next save. A skip, and an update
	// that changed nothing, report the revision the entry already held.
	Revision int
}

// The outcomes SaveContentEntry reports.
const (
	ContentEntryCreated = "created"
	ContentEntryUpdated = "updated"
	ContentEntrySkipped = "skipped"
)

// ContentEntryReader reads entries and their past revisions from the
// editorial store, with the revision numbers SaveContentEntry checks. A
// consumer finds it by asserting it on the writer ContentEntryWriter
// returns. A writer without it predates ExpectedRevision and ignores the
// field, so a consumer that relies on the check refuses to write instead.
//
// Every call is scoped to the tenant on ctx and refuses a context without
// one with ErrTenantRequired. An entry the tenant does not hold, and a
// deleted one, are ErrNotFound.
type ContentEntryReader interface {
	// ContentEntry returns the entry as stored now.
	ContentEntry(ctx context.Context, id uuid.UUID) (ContentEntry, error)

	// ContentEntryRevision returns the entry as it was at revision. A
	// revision the entry never had is ErrNotFound. A revision older than
	// the caller's license lets it read is a *NotGrantedError. Schema and
	// Slug are the entry's current ones, because a revision keeps neither.
	ContentEntryRevision(ctx context.Context, id uuid.UUID, revision int) (ContentEntry, error)
}

// ContentEntry is one entry, or one past revision of it, as
// ContentEntryReader returns it.
type ContentEntry struct {
	ID     uuid.UUID
	Schema string
	Slug   string
	Title  string
	// Status is draft, published or archived.
	Status string
	// Fields are the entry's values, keyed by the schema's field names.
	Fields map[string]any
	// Meta is the entry's metadata beside its fields, such as the values a
	// page's head carries, as the store keeps it. It is nil for a store
	// that keeps no metadata, or none for this entry.
	Meta map[string]any
	// Revision is the number SaveContentEntry takes as ExpectedRevision.
	Revision int
	// UpdatedAt and UpdatedBy say when this revision was written and by
	// whom. UpdatedBy is uuid.Nil when no user was recorded.
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}

// ContentEntryVersionLister lists an entry's past revisions, newest first,
// with who wrote each, when, and the note it carries. A consumer finds it by
// asserting it on the writer ContentEntryWriter returns, as it finds
// ContentEntryReader.
//
// Every call is scoped to the tenant on ctx and refuses a context without
// one with ErrTenantRequired. An entry the tenant does not hold, and a
// deleted one, are ErrNotFound. A query outside its bounds is ErrValidation.
type ContentEntryVersionLister interface {
	// ContentEntryVersions returns one page of the entry's revisions.
	// Revisions older than the caller's license lets it read are left out
	// and counted in Hidden, except the entry's newest, which is always
	// read.
	ContentEntryVersions(ctx context.Context, id uuid.UUID, q ContentEntryVersionQuery) (ContentEntryVersionPage, error)
}

// ContentEntryVersionQuery is the page of revisions to list.
type ContentEntryVersionQuery struct {
	// Before lists only the revisions numbered below it, so Before set to
	// a revision and Limit 1 read the revision that preceded it. 0 lists
	// from the newest.
	Before int
	// Limit is the most revisions the page holds, from 1 to 100, and
	// Offset how many it skips.
	Limit  int
	Offset int
}

// ContentEntryVersionPage is one page of an entry's revisions.
type ContentEntryVersionPage struct {
	// Versions are newest first.
	Versions []ContentEntryVersion
	// Total counts the revisions the query reads, across every page.
	Total int
	// Hidden counts the revisions the query matched that the caller's
	// license keeps it from reading.
	Hidden int
	// Recording says whether a save of the entry records a revision now.
	// A store that keeps a revision only when an administrator turned it
	// on answers false while it is off, with the revisions it kept before.
	Recording bool
}

// ContentEntryVersion is one past revision with what the store recorded
// about the write. UpdatedAt and UpdatedBy say when and by whom.
type ContentEntryVersion struct {
	ContentEntry
	// Note is the label the write gave the revision.
	Note string
	// Action is the kind of write, such as created, updated or rollback,
	// and empty on a revision written before the store recorded one.
	Action string
	// Source is where the write came from, such as console or import, and
	// empty on a revision written before the store recorded one.
	Source string
}

// ContentEntryWriterRegistrar is implemented by the engine host. The plugin
// that owns the editorial store calls it when it activates and again with
// nil when it stops, so the registration follows the plugin's lifecycle,
// not the process. Through ScopedHost a registration needs
// CapContentEntryWriter, and only the plugin that registered the writer may
// replace or clear it. A refusal comes back as the error.
type ContentEntryWriterRegistrar interface {
	RegisterContentEntryWriter(w ContentEntryWriter) error
}

// ContentEntryWriterProvider is implemented by the engine host and forwarded
// by ScopedHost. A consumer asks it per job, never at Start, because the
// owning plugin may start after it. Nil means no plugin has registered a
// writer.
type ContentEntryWriterProvider interface {
	ContentEntryWriter() ContentEntryWriter
}
