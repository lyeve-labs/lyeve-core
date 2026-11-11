package core

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// MediaLibrary lists, uploads and publishes files in the media library: the
// library the admin's media screens show, which scans, stores and keeps a
// record of every file. The plugin that owns the library registers one. A
// plugin that lets a person pick or add files where they edit, outside the
// media screens, goes through it, so a file it adds meets the checks an
// upload from those screens meets and shows in the library like any other.
//
// Every call is scoped to the tenant on ctx and refuses a context without
// one with ErrTenantRequired. A refusal wraps ErrValidation or ErrNotFound
// with words a person can act on, and carries no driver text. Anything else
// is a store failure.
type MediaLibrary interface {
	// ListMedia returns one page of the tenant's files, newest first.
	ListMedia(ctx context.Context, q MediaQuery) (MediaPage, error)

	// UploadMedia stores one file in the library and returns it as stored.
	// A file the library refuses, for its type, its size or its folder, is
	// ErrValidation, and a file the virus scan finds infected is
	// ErrMediaScanFailed. Nothing is stored for either.
	UploadMedia(ctx context.Context, in MediaUpload) (MediaItem, error)

	// PublishMedia publishes a file at a stable public path on the tenant's
	// host and returns it with PublicPath set. Publishing a published file
	// changes nothing and is not an error, also when two calls race. A
	// folder that is not empty must be the file's own folder: a file the
	// tenant does not hold, and one filed in another folder, are both
	// ErrNotFound. A file the library will not publish, such as an image
	// that still records where it was taken, is ErrValidation.
	PublishMedia(ctx context.Context, id uuid.UUID, folder string) (MediaItem, error)
}

// MediaUploadLimiter is implemented by a MediaLibrary that reports its size
// limit, so a caller can refuse a file that is too large before it reads the
// whole body, and can tell a person the limit before they pick a file. It is
// optional: a caller asserts it on the library it holds and keeps a limit of
// its own when the library does not implement it.
type MediaUploadLimiter interface {
	// MaxUploadBytes is the most bytes one file may carry for the tenant on
	// ctx: UploadMedia stores a file of exactly this size and refuses one
	// byte more. A library with one limit for every tenant may ignore ctx.
	// 0 or less reports no limit, and the caller keeps its own.
	MaxUploadBytes(ctx context.Context) int64
}

// ErrMediaScanFailed is the refusal of a file the virus scan found infected.
// It wraps ErrValidation, so a caller that tells the two apart checks for it
// first.
var ErrMediaScanFailed = fmt.Errorf("the file did not pass the virus scan: %w", ErrValidation)

// MediaQuery is the page of files ListMedia returns.
type MediaQuery struct {
	// Folder lists only the files filed in exactly this folder, such as
	// "/brand/logos". Empty lists the whole library.
	Folder string
	// Search keeps the files whose name, alternative text or tags contain
	// it, ignoring case. Empty keeps every file.
	Search string
	// Limit is the most files the page holds: 0 takes 50, and above 200
	// takes 200. Offset is how many files the page skips. A negative value
	// of either is ErrValidation.
	Limit  int
	Offset int
}

// MediaPage is one page of files.
type MediaPage struct {
	Items []MediaItem
	// Total counts the files the query matches, across every page.
	Total int
}

// MediaUpload is one file to store.
type MediaUpload struct {
	// Body is the file's content. The library reads its type from the
	// bytes, never from the name or a declared type.
	Body io.ReadSeeker
	// Filename is the name the file is stored and published under.
	Filename string
	// Folder is the folder the file is filed in. Empty files it at the
	// library's root.
	Folder string
	// ContentTypes are the types the file may be, as read from its bytes,
	// such as "image/png". Empty takes the library's own list.
	ContentTypes []string
	// Tags are stored with the file beside the tags the library derives
	// from it.
	Tags []string
	// AltText describes the file for a reader who cannot see it.
	AltText string
	// MaxBytes lowers the library's size limit for this file when above
	// 0. It never raises it.
	MaxBytes int64
}

// MediaItem is one file of the library.
type MediaItem struct {
	ID       uuid.UUID
	Filename string
	// ContentType is the type read from the file's bytes.
	ContentType string
	Size        int64
	// Width and Height are an image's size in pixels, 0 when unknown.
	Width   int
	Height  int
	Folder  string
	AltText string
	Tags    []string
	// PublicPath is the file's stable path on the tenant's host while it is
	// published, such as "/api/v1/media/<id>/hero.jpg", and empty while it
	// is not.
	PublicPath string
	CreatedAt  time.Time
}

// MediaLibraryRegistrar is implemented by the engine host. The plugin that
// owns the media library calls it when it activates and again with nil when
// it stops. Through ScopedHost a registration needs CapMediaLibrary, and only
// the plugin that registered the library may replace or clear it. A refusal
// comes back as the error.
type MediaLibraryRegistrar interface {
	RegisterMediaLibrary(l MediaLibrary) error
}

// MediaLibraryProvider is implemented by the engine host and forwarded by
// ScopedHost. A consumer asks it per request, never at Start, because the
// owning plugin may start after it. Nil means no plugin has registered a
// library.
type MediaLibraryProvider interface {
	MediaLibrary() MediaLibrary
}
