package core

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"

	"go.opentelemetry.io/otel/trace"
)

// Role interfaces
// Host is composed of smaller role interfaces so plugins can depend on only
// what they use. Type-assert a role interface (e.g. DBHost) instead of the
// full Host when you only need a subset of capabilities. This makes plugin
// dependencies explicit and simplifies testing with partial mocks.
//
// Host itself composes every role, so a plugin that needs most of them can
// take Host whole.

// DBHost provides database access methods.
type DBHost interface {
	Querier(ctx context.Context) Querier
	QuerierRO(ctx context.Context) Querier
	Dialect() string
	RawDB() *sql.DB
	MigrationDB() *sql.DB
}

// ObservableHost provides observability primitives (logging and tracing).
type ObservableHost interface {
	Logger(ctx context.Context) *slog.Logger
	Tracer(name string) trace.Tracer
}

// ConfigHost provides read-only access to server configuration.
type ConfigHost interface {
	Config() Config
	Version() string
}

// HookHost provides lifecycle event subscription and publishing.
type HookHost interface {
	Hooks() HookBus
	HookPublisher() HookPublisher
}

// SchemaHost provides schema DDL lifecycle operations.
type SchemaHost interface {
	Schema() SchemaEngine
}

// ScalingHost provides access to the centralized concurrency primitives.
// WorkerPool defaults to 10 workers. DistLock returns a working advisory lock
// on every install. A test host may return nil.
type ScalingHost interface {
	WorkerPool() *engine.WorkerPool
	GoroutineTracker() *engine.GoroutineTracker
	ParallelEngine() *engine.ParallelEngine
	AsyncHookExecutor() *engine.AsyncHookExecutor
	DistLock(name string) *engine.DistLock
}

// CapabilitySet is a pre-computed snapshot of license entitlements, built once
// when the license is loaded or changed. Plugins read from this snapshot in
// Start() instead of calling HasFeature() repeatedly. The zero value reads as
// an install with no license, as plan and state "free" do.
type CapabilitySet struct {
	Features    map[string]bool `json:"features"` // feature key -> granted
	Plan        string          `json:"plan"`
	State       string          `json:"state"`        // "active", "grace", "expired", "free"
	TenantQuota int             `json:"tenant_quota"` // max tenants. 0 = unlimited
	// Caps is every named capacity ceiling the licensing implementation
	// applies, where 0 is unlimited. Read one through Limit.
	Caps map[string]int `json:"caps,omitempty"`
}

// Limit is the ceiling Caps names for name, where 0 is unlimited. A name Caps
// does not carry is unlimited too, because a ceiling is something a licensing
// implementation states rather than something the engine assumes.
func (c CapabilitySet) Limit(name string) int {
	return c.Caps[name]
}

// LicenseHost provides license-aware feature gating.
// Implemented by the engine host. Plugins type-assert to this interface
// to check entitlements without importing the engine's internal packages.
type LicenseHost interface {
	// Capabilities returns a pre-computed snapshot of the current license
	// entitlements. Plugins call this once in Start() and subscribe to
	// "license.changed" hook events for hot-reload rather than polling.
	// With no license set it answers plan and state "free" and grants only
	// what needs no license.
	Capabilities() CapabilitySet

	// HasFeature reports whether the named feature is granted, by the current
	// license or because it needs none. A feature an operator withheld from
	// the request's tenant is refused.
	HasFeature(ctx context.Context, feature string) bool
}

// ContentWriter exposes write access to the schema engine's per-schema content
// tables, so a plugin that owns an editorial store can mirror its writes into
// the tables the public read routes and GraphQL serve. The id is the source
// record's id, so the mirrored row and the source record stay aligned.
type ContentWriter interface {
	// UpsertContent writes a content row under an explicit id, updating an
	// existing row or inserting a missing one.
	UpsertContent(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) error
	// DeleteContent removes a content row by id, scoped to the caller's tenant.
	DeleteContent(ctx context.Context, schemaName string, id uuid.UUID) error
	// SetContentStatus moves a content row between draft, published and
	// archived, the transition the engine's own publish and unpublish routes
	// perform, so a plugin that decides when an entry goes live (an approval
	// on it, a scheduled date) writes the same column the public read routes
	// filter on. The schema must have draft and publish enabled. A row the
	// tenant does not have, or a schema it never defined, is ErrNotFound. A
	// status outside the three is ErrValidation. The write publishes the
	// after_update lifecycle event with the row as it now reads.
	SetContentStatus(ctx context.Context, schemaName string, id uuid.UUID, status string) error
}

// ContentStatusProjector moves a content row between draft, published and
// archived as SetContentStatus does, with the same errors, and publishes no
// lifecycle event. It is for a plugin that keeps its own record of an entry
// and announces the change itself: a content release moves many entries and
// sends one event per entry once every move has held, and none when the
// release is put back. Through SetContentStatus each move would announce
// itself as well, so a subscriber would see every entry twice, and see moves
// that were later undone. A host that can write content implements it.
type ContentStatusProjector interface {
	ProjectContentStatus(ctx context.Context, schemaName string, id uuid.UUID, status string) error
}

// Content statuses SetContentStatus accepts, the values the _status column
// of a schema with draft and publish enabled may hold.
const (
	ContentStatusDraft     = "draft"
	ContentStatusPublished = "published"
	ContentStatusArchived  = "archived"
)

// ValidContentStatus reports whether status is one of the three content
// statuses.
func ValidContentStatus(status string) bool {
	switch status {
	case ContentStatusDraft, ContentStatusPublished, ContentStatusArchived:
		return true
	}
	return false
}

// Host is the stable interface plugins receive from the engine at Start().
// It composes the role interfaces (DB, Observable, Config, Hook, Schema,
// Scaling) into a single all-in-one interface. A plugin should type-assert
// the specific role interface it needs instead of depending on Host directly.
//
// Breaking changes here are semver-major.
type Host interface {
	DBHost
	ObservableHost
	ConfigHost
	HookHost
	SchemaHost
	ScalingHost
	LicenseHost
}
