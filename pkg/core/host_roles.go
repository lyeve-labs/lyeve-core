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
	// existing row or inserting a missing one. A soft-deleted row under the
	// id is ErrConflict, and the row stays deleted.
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

// ContentCacheFlusher drops the content read caches after a write to
// schemaName's table, on every replica. Only writes through ContentWriter
// flush the lists and rows the read routes cache, so a plugin that writes a
// content table with its own SQL, such as a batch import, calls it once each
// write has committed. Until then a list read before the write keeps serving
// the old rows for the cache's lifetime.
type ContentCacheFlusher interface {
	FlushContentCache(ctx context.Context, schemaName string)
}

// ContentUpdater writes to an existing content row and never creates one, for
// a caller that read a row and then writes it. UpdateContent applies data to
// the row id holds for the caller's tenant. A row that is missing or
// soft-deleted is ErrNotFound and nothing is written, so a delete that lands
// between the read and the write is reported instead of being undone or
// written into. A schema the tenant never defined is ErrNotFound, and a value
// the store refuses is ErrValidation, as for ContentWriter. A host that can
// write content implements it.
type ContentUpdater interface {
	UpdateContent(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) error
}

// ContentReader pages through a schema's generated content table, every row
// the tenant on ctx holds and every status, ordered by id. A soft-deleted row
// is not read, since it is gone for every other reader too. It is for
// a plugin that keeps its own record of entries and has to find the rows
// another writer put in the table before it was listening, such as entries
// created through the Content API.
//
// Each row is the schema's fields keyed by field name, with "id",
// "created_at" and "updated_at" added. A relation that holds a column reads
// under that column's name. A schema the tenant never defined is
// ErrNotFound. after is the last id of the previous page, or uuid.Nil for the
// first. A limit outside 1 to 1000 reads 100.
type ContentReader interface {
	ContentPage(ctx context.Context, schemaName string, after uuid.UUID, limit int) ([]map[string]any, error)
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
