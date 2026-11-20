package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEntryWriter records what it was asked to save.
type fakeEntryWriter struct {
	saved []ContentEntryInput
}

func (f *fakeEntryWriter) SaveContentEntry(ctx context.Context, in ContentEntryInput) (ContentEntrySaved, error) {
	if TenantIDFromCtx(ctx) == "" {
		return ContentEntrySaved{}, ErrTenantRequired
	}
	f.saved = append(f.saved, in)
	return ContentEntrySaved{ID: uuid.New(), Outcome: ContentEntryCreated}, nil
}

func (f *fakeEntryWriter) DeleteContentEntry(context.Context, uuid.UUID) error { return nil }

// entryWriterHost is a stubHost that can hold an entry writer, the way the
// engine host does once the owning plugin has registered one.
type entryWriterHost struct {
	*stubHost
	HostRoleRegistry
	writer ContentEntryWriter
}

func (h *entryWriterHost) RegisterContentEntryWriter(w ContentEntryWriter) error {
	h.writer = w
	return nil
}
func (h *entryWriterHost) ContentEntryWriter() ContentEntryWriter { return h.writer }

// The owning plugin registers through its scoped host and an importing
// plugin reads the same writer through its own, with the input passed
// through untouched.
func TestScopedHost_ContentEntryWriter_RegisteredByOwnerReachesImporter(t *testing.T) {
	inner := &entryWriterHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "content", CapAll)
	importer := NewScopedHost(inner, "bulk-import", CapDBWrite|CapRoutes)

	fake := &fakeEntryWriter{}
	var reg Host = owner
	r, ok := reg.(ContentEntryWriterRegistrar)
	require.True(t, ok, "ScopedHost must satisfy ContentEntryWriterRegistrar through the Host interface")
	r.RegisterContentEntryWriter(fake)

	var host Host = importer
	p, ok := host.(ContentEntryWriterProvider)
	require.True(t, ok, "ScopedHost must satisfy ContentEntryWriterProvider through the Host interface")
	w := p.ContentEntryWriter()
	require.Equal(t, ContentEntryWriter(fake), w)

	in := ContentEntryInput{Schema: "posts", Slug: "hello", Fields: map[string]any{"title": "Hello"}, MatchField: "slug", OnMatch: ContentMatchUpdate}
	saved, err := w.SaveContentEntry(WithTenantID(context.Background(), "acme"), in)
	require.NoError(t, err)
	assert.Equal(t, ContentEntryCreated, saved.Outcome)
	assert.Equal(t, []ContentEntryInput{in}, fake.saved)

	owner.RegisterContentEntryWriter(nil)
	assert.Nil(t, importer.ContentEntryWriter(), "after RegisterContentEntryWriter(nil) the writer must be nil")
}

// A host that cannot provide one answers nil and logs once with the op named.
func TestScopedHost_ContentEntryWriter_NilWhenInnerCannotProvide(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "bulk-import", CapAll)
	for range 3 {
		require.Nil(t, sh.ContentEntryWriter())
	}
	recs := unavailableRecords(t, h)
	require.Len(t, recs, 1, h.buf.String())
	assertField(t, recs[0], "op", "ContentEntryWriter")
}

// fakeEntryStore is a writer that also reads, as a store keeping numbered
// revisions registers it.
type fakeEntryStore struct {
	fakeEntryWriter
	entry ContentEntry
}

func (f *fakeEntryStore) ContentEntry(_ context.Context, id uuid.UUID) (ContentEntry, error) {
	if id != f.entry.ID {
		return ContentEntry{}, ErrNotFound
	}
	return f.entry, nil
}

func (f *fakeEntryStore) ContentEntryRevision(ctx context.Context, id uuid.UUID, revision int) (ContentEntry, error) {
	e, err := f.ContentEntry(ctx, id)
	if err != nil || revision != e.Revision {
		return ContentEntry{}, ErrNotFound
	}
	return e, nil
}

// The fields added for an expected revision default to the meaning a caller
// written before them relied on: no id creates or matches, no expected
// revision checks nothing, no note keeps the store's label.
func TestContentEntryInput_ZeroValuesKeepTheOldMeaning(t *testing.T) {
	var in ContentEntryInput
	assert.Equal(t, uuid.Nil, in.ID)
	assert.Zero(t, in.ExpectedRevision)
	assert.Empty(t, in.Note)
	var saved ContentEntrySaved
	assert.Zero(t, saved.Revision)

	var w ContentEntryWriter = &fakeEntryWriter{}
	_, isReader := w.(ContentEntryReader)
	assert.False(t, isReader, "a writer that only writes is not a reader, so a consumer can tell an older store apart")
}

// A consumer holding only the writer its scoped host hands out reaches the
// reader on it, with no capability of its own.
func TestScopedHost_ContentEntryReader_ReachedThroughTheWriter(t *testing.T) {
	inner := &entryWriterHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "content", CapAll)
	consumer := NewScopedHost(inner, "pages", CapRoutes)

	id := uuid.New()
	fake := &fakeEntryStore{entry: ContentEntry{ID: id, Schema: "pages", Slug: "home", Title: "Home", Status: "draft", Revision: 4}}
	require.NoError(t, owner.RegisterContentEntryWriter(fake))

	w := consumer.ContentEntryWriter()
	require.NotNil(t, w)
	r, ok := w.(ContentEntryReader)
	require.True(t, ok, "the writer the host returns must keep the reader it was registered with")

	ctx := WithTenantID(context.Background(), "acme")
	got, err := r.ContentEntry(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, 4, got.Revision)
	past, err := r.ContentEntryRevision(ctx, id, 4)
	require.NoError(t, err)
	assert.Equal(t, "Home", past.Title)
	_, err = r.ContentEntry(ctx, uuid.New())
	assert.ErrorIs(t, err, ErrNotFound)

	in := ContentEntryInput{Schema: "pages", ID: id, ExpectedRevision: 4, Note: "edited", Fields: map[string]any{"title": "Home"}}
	_, err = w.SaveContentEntry(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, []ContentEntryInput{in}, fake.saved, "the id, expected revision and note reach the store untouched")
}

// A consumer that compares a write with the stored entry reads the entry's
// meta beside its fields, so a value kept in meta can be compared too.
func TestScopedHost_ContentEntryReader_CarriesMeta(t *testing.T) {
	inner := &entryWriterHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "content", CapAll)
	consumer := NewScopedHost(inner, "pages", CapRoutes)

	id := uuid.New()
	meta := map[string]any{"contact": "ada@example.com", "seo": map[string]any{"title": "Home"}}
	fake := &fakeEntryStore{entry: ContentEntry{ID: id, Schema: "pages", Revision: 2, Fields: map[string]any{"title": "Home"}, Meta: meta}}
	require.NoError(t, owner.RegisterContentEntryWriter(fake))

	r, ok := consumer.ContentEntryWriter().(ContentEntryReader)
	require.True(t, ok)
	ctx := WithTenantID(context.Background(), "acme")
	got, err := r.ContentEntry(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, meta, got.Meta)
	past, err := r.ContentEntryRevision(ctx, id, 2)
	require.NoError(t, err)
	assert.Equal(t, meta, past.Meta)

	var none ContentEntry
	assert.Nil(t, none.Meta, "an entry a store read without meta holds none")
}

// fakeVersionStore is a store that also lists an entry's revisions.
type fakeVersionStore struct {
	fakeEntryStore
	asked []ContentEntryVersionQuery
	page  ContentEntryVersionPage
}

func (f *fakeVersionStore) ContentEntryVersions(_ context.Context, id uuid.UUID, q ContentEntryVersionQuery) (ContentEntryVersionPage, error) {
	if id != f.entry.ID {
		return ContentEntryVersionPage{}, ErrNotFound
	}
	f.asked = append(f.asked, q)
	return f.page, nil
}

// A consumer reaches the version list on the writer its scoped host hands
// out, as it reaches the reader, and a writer without one is told apart.
func TestScopedHost_ContentEntryVersionLister_ReachedThroughTheWriter(t *testing.T) {
	inner := &entryWriterHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "content", CapAll)
	consumer := NewScopedHost(inner, "pages", CapRoutes)

	id := uuid.New()
	author := uuid.New()
	fake := &fakeVersionStore{
		fakeEntryStore: fakeEntryStore{entry: ContentEntry{ID: id, Schema: "pages", Revision: 7}},
		page: ContentEntryVersionPage{
			Versions: []ContentEntryVersion{
				{ContentEntry: ContentEntry{ID: id, Revision: 7, UpdatedBy: author}, Note: "Before the launch", Action: "updated", Source: "import"},
				{ContentEntry: ContentEntry{ID: id, Revision: 5, UpdatedBy: author}, Action: "created"},
			},
			Total: 2, Hidden: 1, Recording: true,
		},
	}
	require.NoError(t, owner.RegisterContentEntryWriter(fake))

	l, ok := consumer.ContentEntryWriter().(ContentEntryVersionLister)
	require.True(t, ok, "the writer the host returns must keep the version list it was registered with")
	ctx := WithTenantID(context.Background(), "acme")
	got, err := l.ContentEntryVersions(ctx, id, ContentEntryVersionQuery{Before: 7, Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, fake.page, got)
	assert.Equal(t, []ContentEntryVersionQuery{{Before: 7, Limit: 1}}, fake.asked, "the query reaches the store untouched")
	_, err = l.ContentEntryVersions(ctx, uuid.New(), ContentEntryVersionQuery{Limit: 1})
	assert.ErrorIs(t, err, ErrNotFound)

	var older ContentEntryWriter = &fakeEntryStore{}
	_, lists := older.(ContentEntryVersionLister)
	assert.False(t, lists, "a store that reads but does not list is told apart")
}

// guardedEntryWriter is a writer that also deletes at a revision, as the
// editorial store registers it.
type guardedEntryWriter struct {
	fakeEntryWriter
	deletedAt map[uuid.UUID]int
}

func (f *guardedEntryWriter) DeleteContentEntryAt(ctx context.Context, id uuid.UUID, revision int) error {
	if TenantIDFromCtx(ctx) == "" {
		return ErrTenantRequired
	}
	f.deletedAt[id] = revision
	return nil
}

// A consumer holding only the writer its scoped host hands out reaches the
// revision-guarded delete on it, with the id and the revision untouched.
func TestScopedHost_ContentEntryRevisionDeleter_ReachedThroughTheWriter(t *testing.T) {
	inner := &entryWriterHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "content", CapAll)
	consumer := NewScopedHost(inner, "pages", CapRoutes)

	fake := &guardedEntryWriter{deletedAt: map[uuid.UUID]int{}}
	require.NoError(t, owner.RegisterContentEntryWriter(fake))

	d, ok := consumer.ContentEntryWriter().(ContentEntryRevisionDeleter)
	require.True(t, ok, "the writer the host returns must keep the guarded delete it was registered with")
	id := uuid.New()
	require.NoError(t, d.DeleteContentEntryAt(WithTenantID(context.Background(), "acme"), id, 3))
	assert.Equal(t, map[uuid.UUID]int{id: 3}, fake.deletedAt)
}
