package core

import (
	"context"
	"io"
	"time"
)

// StorageObject describes a stored media asset.
type StorageObject struct {
	Key          string    `json:"key"`
	ContentType  string    `json:"content_type"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag,omitempty"`
	LastModified time.Time `json:"last_modified"`
}

// Storage is the interface that every storage driver must satisfy.
// The engine provides a concrete implementation (local FS, S3, etc.)
// and exposes it to plugins via StorageProvider on the Host.
type Storage interface {
	// Put stores r under key with the given content-type and returns the
	// resulting StorageObject.
	Put(ctx context.Context, key, contentType string, r io.Reader) (*StorageObject, error)

	// Get returns a reader for the object at key.
	Get(ctx context.Context, key string) (io.ReadCloser, *StorageObject, error)

	// Delete removes the object at key. Implementations must not error on
	// missing keys.
	Delete(ctx context.Context, key string) error

	// SignedURL returns a time-limited pre-signed download URL for key.
	// Returns an empty string when the driver does not support signed URLs.
	SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)

	// List returns up to limit objects with the given prefix.
	List(ctx context.Context, prefix string, limit int) ([]*StorageObject, error)
}

// StorageProvider is an optional interface that engine Host implementations
// implement to expose their storage backend to plugins that need blob storage
// (e.g. media uploads). Plugins type-assert the host to this interface in
// their Start() method.
type StorageProvider interface {
	Storage() Storage
}

// StorageSetter allows a plugin to replace the default storage backend
// (e.g., swap Local for S3 after activation).
type StorageSetter interface {
	SetStorage(Storage)
}
