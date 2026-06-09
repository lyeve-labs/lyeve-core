// Package provider is the unified provider framework for LyEve CMS. It puts
// backend providers under a single registry with auto-detection, health checks
// and per-operation cost tracking. The built-in providers are the database
// adapters and the S3-compatible storage clients.
//
// Each category has its own sub-interface that embeds the base Provider
// interface, keeping the API surface clean for consumers that only need one
// category while still allowing cross-cutting operations (health dashboard,
// cost analytics) to work generically.
//
// Storage providers self-register via init(), and RegisterDBProviders adds the
// database ones.
package provider

import (
	"context"
	"time"
)

// Category identifies what kind of backend a provider serves.
type Category string

const (
	CategoryDB      Category = "db"
	CategoryStorage Category = "storage"
)

// Provider is the base interface every backend provider implements.
// Category-specific interfaces (DBProvider, StorageProvider) embed this
// and add domain-specific methods.
type Provider interface {
	// Name returns the canonical provider name (e.g. "postgres", "s3").
	Name() string

	// Category returns the provider category.
	Category() Category

	// Capabilities returns the feature flags this provider supports.
	// Each category defines its own bitmask constants. Check with Has().
	Capabilities() Capabilities

	// HealthCheck verifies connectivity to the backend. Implementations should
	// use the context deadline as a timeout.
	HealthCheck(ctx context.Context) error

	// AutoDetect attempts to detect this provider from a DSN/URL/config string.
	// Returns true when the string matches this provider (e.g. postgres://... ->
	// true for the postgres provider, false for mysql).
	AutoDetect(dsn string) bool
}

// Capabilities is a generic bitmask of feature flags. Each category defines
// its own set of flags as uint64 constants. Use Has() to check membership.
type Capabilities struct {
	mask uint64
}

// NewCapabilities builds a Capabilities set from one or more flags.
func NewCapabilities(caps ...uint64) Capabilities {
	var m uint64
	for _, c := range caps {
		m |= c
	}
	return Capabilities{mask: m}
}

// Has reports whether all given capability flags are present.
func (c Capabilities) Has(need uint64) bool {
	return c.mask&need == need
}

// Mask returns the raw bitmask for debugging.
func (c Capabilities) Mask() uint64 { return c.mask }

// HealthStatus summarizes the result of a health check.
type HealthStatus struct {
	Provider  string        `json:"provider"`
	Category  Category      `json:"category"`
	Healthy   bool          `json:"healthy"`
	Latency   time.Duration `json:"latency_ms"`
	Error     string        `json:"error,omitempty"`
	CheckedAt time.Time     `json:"checked_at"`
}

// HealthChecker is an optional interface for providers that support
// richer health diagnostics beyond a simple ping.
type HealthChecker interface {
	// DetailedHealth returns a structured health report beyond pass/fail.
	DetailedHealth(ctx context.Context) HealthStatus
}

// ConnectionString is the minimal info needed to connect to a provider.
type ConnectionString struct {
	Scheme   string            // postgres, mysql, s3
	Host     string            // hostname or IP
	Port     int               // port number
	DBName   string            // database/keyspace/bucket/index name
	User     string            // username or access key
	Password string            // password or secret key
	Params   map[string]string // additional query parameters
	Raw      string            // original connection string
}

// OperationCost records resource usage for a single backend operation.
// Collected per-operation and aggregated by the cost tracker.
type OperationCost struct {
	Provider  string        `json:"provider"`
	Category  Category      `json:"category"`
	Operation string        `json:"operation"`           // "query", "get", "put"
	BytesIn   int64         `json:"bytes_in,omitempty"`  // bytes read
	BytesOut  int64         `json:"bytes_out,omitempty"` // bytes written
	Latency   time.Duration `json:"latency_ms"`
	Error     string        `json:"error,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
}

// CostTracker accumulates and reports operation costs across all providers.
type CostTracker interface {
	// Record logs a single operation's cost.
	Record(cost OperationCost)

	// Snapshot returns cumulative costs since last Snapshot and resets counters.
	// The second return value is the time range the snapshot covers.
	Snapshot() ([]OperationCost, time.Time, time.Time)

	// Total returns cumulative costs without resetting.
	Total() []OperationCost
}
