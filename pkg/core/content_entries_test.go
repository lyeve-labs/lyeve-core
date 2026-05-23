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
	writer ContentEntryWriter
}

func (h *entryWriterHost) RegisterContentEntryWriter(w ContentEntryWriter) { h.writer = w }
func (h *entryWriterHost) ContentEntryWriter() ContentEntryWriter          { return h.writer }

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
