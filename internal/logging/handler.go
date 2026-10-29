package logging

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// staticLevelManager is a LevelManager that returns a fixed level.
type staticLevelManager struct{ level slog.Level }

func (m *staticLevelManager) Level(_, _ string) slog.Level { return m.level }
func (m *staticLevelManager) DefaultLevel() slog.Level     { return m.level }

// EnrichHandler: context-aware slog.Handler

// EnrichHandler is a slog.Handler that decorates every log record with
// attributes derived from the context (trace-id, span-id, tenant-id, request-id,
// plugin name). It wraps one or more inner handlers.
//
// Unlike slog.Handler.WithAttrs (which enriches at construction time), this
// handler enriches at Handle time so per-request values are always current.
//
// On each Handle call, the enriched record is:
//  1. Broadcast to the global LogTailer (SSE subscribers) if set.
//  2. Written to the configured LogSink (DB, Loki, ES, etc.) if set.
//  3. Delegated to the inner slog.Handler (stdout JSON).
type EnrichHandler struct {
	inner slog.Handler
	state *handlerState
}

// handlerState holds the level manager and sink that a root handler shares
// with every handler derived from it, so a logger derived before the logging
// plugin starts follows the level and reaches the sink it sets later. Both
// are read on every log call, so they are atomic pointers rather than fields
// behind a lock.
type handlerState struct {
	leveler atomic.Pointer[levelerRef]
	sink    atomic.Pointer[sinkRef]
}

type levelerRef struct{ LevelManager }

type sinkRef struct{ observability.LogSink }

// NewEnrichHandler creates a new EnrichHandler wrapping the given inner handler.
func NewEnrichHandler(inner slog.Handler, leveler LevelManager) *EnrichHandler {
	if leveler == nil {
		leveler = &staticLevelManager{level: slog.LevelInfo}
	}
	h := &EnrichHandler{inner: inner, state: &handlerState{}}
	h.state.leveler.Store(&levelerRef{leveler})
	return h
}

// NewDefaultHandler creates an EnrichHandler writing JSON to os.Stdout at Info level.
func NewDefaultHandler() *EnrichHandler {
	return NewEnrichHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		nil,
	)
}

// SetLeveler replaces the level manager for this handler and every handler
// derived from it. Safe for concurrent use.
func (h *EnrichHandler) SetLeveler(leveler LevelManager) {
	if leveler == nil {
		leveler = &staticLevelManager{level: slog.LevelInfo}
	}
	h.state.leveler.Store(&levelerRef{leveler})
}

// Leveler returns the current level manager.
func (h *EnrichHandler) Leveler() LevelManager {
	return h.state.leveler.Load().LevelManager
}

// SetSink attaches a LogSink for persistent / external log delivery, for this
// handler and every handler derived from it. Every parsed record is written
// to the sink. Set to nil to detach.
func (h *EnrichHandler) SetSink(sink observability.LogSink) {
	if sink == nil {
		h.state.sink.Store(nil)
		return
	}
	h.state.sink.Store(&sinkRef{sink})
}

// Sink returns the currently attached LogSink, or nil.
func (h *EnrichHandler) Sink() observability.LogSink {
	if ref := h.state.sink.Load(); ref != nil {
		return ref.LogSink
	}
	return nil
}

// Enabled reports whether the handler is interested in the record.
func (h *EnrichHandler) Enabled(ctx context.Context, level slog.Level) bool {
	leveler := h.Leveler()

	tenant := tenantFromCtx(ctx)
	plugin := PluginFromCtx(ctx)
	effective := leveler.Level(tenant, plugin)
	return level >= effective
}

// Handle enriches the record with context attributes, broadcasts to
// tail subscribers, writes to the sink, then delegates to the inner handler.
func (h *EnrichHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := EnrichAttrs(ctx)

	tenant := tenantFromCtx(ctx)
	plugin := PluginFromCtx(ctx)

	// The console line follows the stored entry's rule below, so each key
	// appears once: a context attribute gives way to the record's own value,
	// and tenant_id and plugin come from the context alone, last. EnrichAttrs
	// names tenant_id and plugin too, so those are left to this handler.
	own := make(map[string]bool, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		own[a.Key] = true
		return true
	})
	enriched := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	for _, a := range attrs {
		if !own[a.Key] && a.Key != "tenant_id" && a.Key != "plugin" {
			enriched.AddAttrs(a)
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		if (a.Key == "tenant_id" && tenant != "") || (a.Key == "plugin" && plugin != "") {
			return true
		}
		enriched.AddAttrs(a)
		return true
	})
	if tenant != "" {
		enriched.AddAttrs(slog.String("tenant_id", tenant))
	}
	if plugin != "" {
		enriched.AddAttrs(slog.String("plugin", plugin))
	}

	// The record's own attributes belong in the stored entry too, not only in
	// the console. The stored log is the one an operator searches, and a sink
	// that redacts fields can only redact the fields that reach it.
	//
	// Context attrs first, then the record's, so a caller's value wins where
	// both name the same key. tenant and plugin are applied last: those are
	// facts about the request, not something a log line may claim.
	entryAttrs := make(map[string]any, len(attrs)+r.NumAttrs()+2)
	for _, a := range attrs {
		entryAttrs[a.Key] = attrValue(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		entryAttrs[a.Key] = attrValue(a)
		return true
	})
	if tenant != "" {
		entryAttrs["tenant_id"] = tenant
	}
	if plugin != "" {
		entryAttrs["plugin"] = plugin
	}
	entry := observability.LogEntry{
		Timestamp: r.Time.UTC().Format(time.RFC3339Nano),
		Level:     r.Level.String(),
		Message:   r.Message,
		Attrs:     entryAttrs,
	}

	if tailer := observability.GlobalTailer(); tailer != nil {
		tailer.Broadcast(entry)
	}

	if sink := h.Sink(); sink != nil {
		sink.Write(entry)
	}

	return h.inner.Handle(ctx, enriched)
}

// attrValue resolves an attribute to something JSON can carry.
//
// The commonest attribute in this codebase is an error, and encoding/json renders
// most error types as {} - so storing the value as-is would have replaced one
// kind of missing detail with another. Stringer covers the next commonest,
// url.URL and time.Duration among them.
func attrValue(a slog.Attr) any {
	v := a.Value.Resolve().Any()
	switch t := v.(type) {
	case error:
		return t.Error()
	case fmt.Stringer:
		return t.String()
	default:
		return v
	}
}

// WithAttrs returns a new EnrichHandler with pre-added attributes.
func (h *EnrichHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &EnrichHandler{
		inner: h.inner.WithAttrs(attrs),
		state: h.state,
	}
}

// WithGroup returns a new EnrichHandler with grouped attributes.
func (h *EnrichHandler) WithGroup(name string) slog.Handler {
	return &EnrichHandler{
		inner: h.inner.WithGroup(name),
		state: h.state,
	}
}

// Logger creates a *slog.Logger backed by this handler.
func (h *EnrichHandler) Logger() *slog.Logger {
	return slog.New(h)
}

// Tenant extraction: uses core.TenantContextKey (which aliases tenant.Key).

func tenantFromCtx(ctx context.Context) string {
	if v := ctx.Value(core.TenantContextKey); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// Global enrich handler

var globalEnrichHandler atomic.Pointer[EnrichHandler]

// SetGlobalEnrichHandler stores h as the process-wide enrich handler.
func SetGlobalEnrichHandler(h *EnrichHandler) {
	globalEnrichHandler.Store(h)
}

// GlobalEnrichHandler returns the process-wide enrich handler, or nil.
func GlobalEnrichHandler() *EnrichHandler {
	return globalEnrichHandler.Load()
}

// Middleware enriches every HTTP request with an EnrichHandler-backed logger.
func Middleware(handler *EnrichHandler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger := slog.New(handler)
			ctx := r.Context()
			ctx = WithLogger(ctx, logger)

			if reqID := chimw.GetReqID(ctx); reqID != "" {
				ctx = WithRequestID(ctx, reqID)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
