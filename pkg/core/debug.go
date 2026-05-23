package core

import (
	"context"
	"database/sql"
	"time"
)

// DebugRecorder is the contract plugins and middleware use to record debug
// segments when the per-request debug tracer is active. Implementations
// must be safe for concurrent use (multiple goroutines may record segments
// during a single request: e.g. HTTP handler + background hook dispatch).
type DebugRecorder interface {
	// RecordQuery records a single DB query execution with timing.
	// kind is the segment kind: "db_query", "middleware", "plugin", "hook_event".
	RecordQuery(name string, kind string, dur time.Duration, sql string, args []any)
}

// DebugQuerierFunc is set by internal/debug at init time to wrap the
// engine's Querier with debug recording when a tracer is present in ctx.
// When nil (default), debug wrapping is disabled.
// dialect and rawDB are provided by the engine host so the debug querier
// can run EXPLAIN against the query plan.
var DebugQuerierFunc func(ctx context.Context, q Querier, dialect string, rawDB *sql.DB) Querier

// DebugHookBusFunc is set by internal/debug at init time to wrap the
// engine's HookBus with debug recording. When nil (default), no wrapping.
var DebugHookBusFunc func(inner HookBus) HookBus

// DebugHookPublisherFunc is set by internal/debug at init time to wrap
// the engine's HookPublisher with debug recording. When nil (default),
// no wrapping.
var DebugHookPublisherFunc func(inner HookPublisher) HookPublisher
