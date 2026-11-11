package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMediaLibrary records the uploads it was asked to store.
type fakeMediaLibrary struct {
	uploads []MediaUpload
}

func (f *fakeMediaLibrary) ListMedia(ctx context.Context, _ MediaQuery) (MediaPage, error) {
	if TenantIDFromCtx(ctx) == "" {
		return MediaPage{}, ErrTenantRequired
	}
	return MediaPage{}, nil
}

func (f *fakeMediaLibrary) UploadMedia(ctx context.Context, in MediaUpload) (MediaItem, error) {
	if TenantIDFromCtx(ctx) == "" {
		return MediaItem{}, ErrTenantRequired
	}
	f.uploads = append(f.uploads, in)
	return MediaItem{ID: uuid.New(), Filename: in.Filename, Folder: in.Folder}, nil
}

func (f *fakeMediaLibrary) PublishMedia(context.Context, uuid.UUID, string) (MediaItem, error) {
	return MediaItem{}, ErrNotFound
}

// mediaLibraryHost is a stubHost that can hold a media library, the way the
// engine host does once the owning plugin has registered one.
type mediaLibraryHost struct {
	*stubHost
	HostRoleRegistry
	library MediaLibrary
}

func (h *mediaLibraryHost) RegisterMediaLibrary(l MediaLibrary) error {
	h.library = l
	return nil
}
func (h *mediaLibraryHost) MediaLibrary() MediaLibrary { return h.library }

// The owning plugin registers through its scoped host and a consuming plugin
// reaches the same library through its own, with no grant of its own, until
// the owner clears it.
func TestScopedHost_MediaLibrary_RegisteredByOwnerReachesConsumer(t *testing.T) {
	inner := &mediaLibraryHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "library-owner", CapDBWrite|CapMediaLibrary)
	consumer := NewScopedHost(inner, "editor", CapRoutes)

	fake := &fakeMediaLibrary{}
	var reg Host = owner
	r, ok := reg.(MediaLibraryRegistrar)
	require.True(t, ok, "ScopedHost must satisfy MediaLibraryRegistrar through the Host interface")
	require.NoError(t, r.RegisterMediaLibrary(fake))

	var host Host = consumer
	p, ok := host.(MediaLibraryProvider)
	require.True(t, ok, "ScopedHost must satisfy MediaLibraryProvider through the Host interface")
	l := p.MediaLibrary()
	require.Equal(t, MediaLibrary(fake), l)

	in := MediaUpload{Filename: "hero.jpg", Folder: "/pages/home", ContentTypes: []string{"image/jpeg"}}
	_, err := l.UploadMedia(WithTenantID(context.Background(), "acme"), in)
	require.NoError(t, err)
	assert.Equal(t, []MediaUpload{in}, fake.uploads)

	require.NoError(t, owner.RegisterMediaLibrary(nil))
	assert.Nil(t, consumer.MediaLibrary(), "after RegisterMediaLibrary(nil) the library must be nil")
}

// A plugin without the grant can neither install a library nor clear the one
// the owner registered.
func TestScopedHost_MediaLibrary_RefusedWithoutTheGrant(t *testing.T) {
	inner := &mediaLibraryHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "library-owner", CapMediaLibrary)
	intruder := NewScopedHost(inner, "intruder", CapAll&^CapMediaLibrary)

	fake := &fakeMediaLibrary{}
	require.NoError(t, owner.RegisterMediaLibrary(fake))

	err := intruder.RegisterMediaLibrary(&fakeMediaLibrary{})
	require.ErrorIs(t, err, ErrCapDenied)
	assert.ErrorContains(t, err, "CapMediaLibrary", "the refusal names the grant the plugin lacks")
	require.ErrorIs(t, intruder.RegisterMediaLibrary(nil), ErrCapDenied)
	assert.Equal(t, MediaLibrary(fake), inner.MediaLibrary(), "a plugin without the grant replaced or cleared the library")
}

// A host that cannot provide one answers nil and logs once with the op named.
func TestScopedHost_MediaLibrary_NilWhenInnerCannotProvide(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "editor", CapAll)
	for range 3 {
		require.Nil(t, sh.MediaLibrary())
	}
	recs := unavailableRecords(t, h)
	require.Len(t, recs, 1, h.buf.String())
	assertField(t, recs[0], "op", "MediaLibrary")
}

// The scan refusal is a validation refusal, so a caller that maps only
// ErrValidation still answers the person and not an outage.
func TestErrMediaScanFailed_IsAValidationRefusal(t *testing.T) {
	assert.ErrorIs(t, ErrMediaScanFailed, ErrValidation)
	assert.NotErrorIs(t, ErrValidation, ErrMediaScanFailed)
}

// A library is a role capability, so a plugin's declaration never narrows it
// away from the plugin the build appoints.
func TestCapMediaLibrary_IsARoleCapability(t *testing.T) {
	assert.True(t, CapRoles.Has(CapMediaLibrary))
	assert.True(t, CapAll.Has(CapMediaLibrary))
	assert.Equal(t, "CapMediaLibrary", capName(CapMediaLibrary))
}
