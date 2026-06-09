package provider

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// StorageProvider extends Provider with storage-specific operations.
type StorageProvider interface {
	Provider

	// Connect opens a connection to the storage backend using the given config.
	// config is a map of provider-specific key/value pairs (bucket, region,
	// access_key, secret_key, endpoint, etc.).
	Connect(ctx context.Context, config StorageConfig) (StorageClient, error)
}

// StorageConfig carries the parameters for connecting to a storage backend.
type StorageConfig struct {
	// Bucket is the bucket/container name.
	Bucket string

	// Region is the cloud region (e.g. "us-east-1").
	Region string

	// AccessKey is the access key ID / account name.
	AccessKey string

	// SecretKey is the secret access key / account key.
	SecretKey string

	// Endpoint is the custom endpoint URL (for MinIO, etc.).
	// When empty, uses the provider's default.
	Endpoint string

	// UseSSL enables HTTPS transport.
	UseSSL bool

	// ForcePathStyle enables path-style addressing (required by MinIO).
	ForcePathStyle bool

	// CDNBaseURL is the public CDN base URL for generating public access URLs.
	CDNBaseURL string

	// MultipartThresholdMB is the file size in MB above which multipart
	// upload is used. Default 5.
	MultipartThresholdMB int64

	// HTTPTimeout is the HTTP client timeout for storage API calls.
	// 0 uses the default (30s).
	HTTPTimeout time.Duration `json:"-"`
}

// StorageClient is the operational interface for a connected storage backend.
// It mirrors core.Storage so existing code works unchanged.
type StorageClient interface {
	// Put stores r under key with the given content-type.
	Put(ctx context.Context, key, contentType string, r io.Reader) (*core.StorageObject, error)

	// Get returns a reader for the object at key.
	Get(ctx context.Context, key string) (io.ReadCloser, *core.StorageObject, error)

	// Delete removes the object at key. Must not error on missing keys.
	Delete(ctx context.Context, key string) error

	// SignedURL returns a time-limited pre-signed download URL.
	// Returns empty string when unsupported.
	SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)

	// List returns up to limit objects with the given prefix.
	List(ctx context.Context, prefix string, limit int) ([]*core.StorageObject, error)

	// Ping verifies connectivity.
	Ping(ctx context.Context) error

	// Close releases resources.
	Close() error
}

// Storage capability flags.
const (
	StorageCapSignedURL         uint64 = 1 << iota // supports pre-signed URLs
	StorageCapMultipart                            // supports multipart upload
	StorageCapLifecycle                            // supports lifecycle rules
	StorageCapVersioning                           // supports object versioning
	StorageCapEncryptionAtRest                     // supports server-side encryption
	StorageCapCDN                                  // CDN integration
	StorageCapEventNotification                    // object event notifications
	StorageCapCrossRegion                          // cross-region replication
)

// detectStorageFromDSN determines which storage provider a URL targets.
func detectStorageFromDSN(dsn string) string {
	dsnLower := strings.ToLower(dsn)
	if strings.HasPrefix(dsnLower, "s3://") {
		if strings.Contains(dsnLower, "minio") {
			return "minio"
		}
		return "s3"
	}
	return ""
}

// StorageConnected wraps a StorageClient with provider metadata and cost tracking.
type StorageConnected struct {
	Client   StorageClient
	Provider StorageProvider
	Costs    CostTracker
}

func (c *StorageConnected) HealthCheck(ctx context.Context) error {
	return c.Client.Ping(ctx)
}

// RecordOp tracks a storage operation in the cost tracker.
func (c *StorageConnected) RecordOp(operation string, bytesIn, bytesOut int64, latency time.Duration, err error) {
	cost := OperationCost{
		Provider:  c.Provider.Name(),
		Category:  CategoryStorage,
		Operation: operation,
		BytesIn:   bytesIn,
		BytesOut:  bytesOut,
		Latency:   latency,
		Timestamp: time.Now(),
	}
	if err != nil {
		cost.Error = err.Error()
	}
	c.Costs.Record(cost)
}

// Put wraps Client.Put with cost tracking.
func (c *StorageConnected) Put(ctx context.Context, key, contentType string, r io.Reader) (*core.StorageObject, error) {
	start := time.Now()
	obj, err := c.Client.Put(ctx, key, contentType, r)
	var bytesOut int64
	if obj != nil {
		bytesOut = obj.Size
	}
	c.RecordOp("put", 0, bytesOut, time.Since(start), err)
	return obj, err
}

// Get wraps Client.Get with cost tracking.
func (c *StorageConnected) Get(ctx context.Context, key string) (io.ReadCloser, *core.StorageObject, error) {
	start := time.Now()
	rc, obj, err := c.Client.Get(ctx, key)
	var bytesIn int64
	if obj != nil {
		bytesIn = obj.Size
	}
	c.RecordOp("get", bytesIn, 0, time.Since(start), err)
	return rc, obj, err
}

// Delete wraps Client.Delete with cost tracking.
func (c *StorageConnected) Delete(ctx context.Context, key string) error {
	start := time.Now()
	err := c.Client.Delete(ctx, key)
	c.RecordOp("delete", 0, 0, time.Since(start), err)
	return err
}
