package logging

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
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
	inner   slog.Handler
	mu      sync.RWMutex
	leveler LevelManager
	sink    observability.LogSink
}

// NewEnrichHandler creates a new EnrichHandler wrapping the given inner handler.
func NewEnrichHandler(inner slog.Handler, leveler LevelManager) *EnrichHandler {
	if leveler == nil {
		leveler = &staticLevelManager{level: slog.LevelInfo}
	}
	return &EnrichHandler{inner: inner, leveler: leveler}
}

// NewDefaultHandler creates an EnrichHandler writing JSON to os.Stdout at Info level.
func NewDefaultHandler() *EnrichHandler {
	return NewEnrichHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
		nil,
	)
}

// SetLeveler replaces the level manager. Safe for concurrent use.
func (h *EnrichHandler) SetLeveler(leveler LevelManager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if leveler == nil {
		leveler = &staticLevelManager{level: slog.LevelInfo}
	}
	h.leveler = leveler
}

// Leveler returns the current level manager.
func (h *EnrichHandler) Leveler() LevelManager {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.leveler
}

// SetSink attaches a LogSink for persistent / external log delivery.
// Every parsed record is written to the sink. Set to nil to detach.
func (h *EnrichHandler) SetSink(sink observability.LogSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sink = sink
}

// Sink returns the currently attached LogSink, or nil.
func (h *EnrichHandler) Sink() observability.LogSink {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sink
}

// Enabled reports whether the handler is interested in the record.
func (h *EnrichHandler) Enabled(ctx context.Context, level slog.Level) bool {
	h.mu.RLock()
	leveler := h.leveler
	h.mu.RUnlock()

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
	if tenant != "" {
		attrs = append(attrs, slog.String("tenant_id", tenant))
	}
	plugin := PluginFromCtx(ctx)
	if plugin != "" {
		attrs = append(attrs, slog.String("plugin", plugin))
	}

	enriched := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	enriched.AddAttrs(attrs...)
	r.Attrs(func(a slog.Attr) bool {
		enriched.AddAttrs(a)
		return true
	})

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

	h.mu.RLock()
	sink := h.sink
	h.mu.RUnlock()
	if sink != nil {
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
		inner:   h.inner.WithAttrs(attrs),
		leveler: h.leveler,
		sink:    h.sink,
	}
}

// WithGroup returns a new EnrichHandler with grouped attributes.
func (h *EnrichHandler) WithGroup(name string) slog.Handler {
	return &EnrichHandler{
		inner:   h.inner.WithGroup(name),
		leveler: h.leveler,
		sink:    h.sink,
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
