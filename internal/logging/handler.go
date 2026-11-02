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
	"github.com/lyeve-labs/lyeve-core/internal/logstream"
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
	// derived holds what logger.With added, keyed under the groups open at
	// the time. It stays here rather than in inner because the stored entry
	// and the tail are built in Handle, and one precedence rule then decides
	// every key for the console, the ring, the sink and the tail alike.
	derived []slog.Attr
	// group is the open groups as "a.b.", the prefix of every later key.
	group string
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

	// The caller's attributes are the derived logger's, then the record's,
	// all under the open groups. A record's value wins over a derived one.
	// A logger with neither derived fields nor a group has only the record's,
	// which are read in place, so the line every request writes pays for no
	// merge.
	plain := len(h.derived) == 0 && h.group == ""
	var own map[string]bool
	var caller []slog.Attr
	if plain {
		own = make(map[string]bool, r.NumAttrs())
		r.Attrs(func(a slog.Attr) bool {
			own[a.Key] = true
			return true
		})
	} else {
		own, caller = h.mergeCaller(r)
	}

	// The console line follows the stored entry's rule below, so each key
	// appears once: a context attribute gives way to the caller's value,
	// and tenant_id and plugin come from the context alone, last. EnrichAttrs
	// names tenant_id and plugin too, so those are left to this handler.
	enriched := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	for _, a := range attrs {
		if !own[a.Key] && a.Key != "tenant_id" && a.Key != "plugin" {
			enriched.AddAttrs(a)
		}
	}
	addCaller := func(a slog.Attr) bool {
		if (a.Key != "tenant_id" || tenant == "") && (a.Key != "plugin" || plugin == "") {
			enriched.AddAttrs(a)
		}
		return true
	}
	if plain {
		r.Attrs(addCaller)
	}
	for _, a := range caller {
		addCaller(a)
	}
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
	entryAttrs := make(map[string]any, len(attrs)+r.NumAttrs()+len(h.derived)+2)
	for _, a := range attrs {
		entryAttrs[a.Key] = storedValue(a)
	}
	storeCaller := func(a slog.Attr) bool {
		entryAttrs[a.Key] = storedValue(a)
		return true
	}
	if plain {
		r.Attrs(storeCaller)
	}
	for _, a := range caller {
		storeCaller(a)
	}
	if tenant != "" {
		entryAttrs["tenant_id"] = logstream.Clip(tenant, logstream.MaxValueBytes)
	}
	if plugin != "" {
		entryAttrs["plugin"] = logstream.Clip(plugin, logstream.MaxValueBytes)
	}
	entry := observability.LogEntry{
		Timestamp: r.Time.UTC().Format(time.RFC3339Nano),
		Level:     r.Level.String(),
		Message:   logstream.Clip(r.Message, logstream.MaxMessageBytes),
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

// mergeCaller returns the derived attributes the record does not override,
// then the record's, every key under the open groups, and the set of keys
// they name.
func (h *EnrichHandler) mergeCaller(r slog.Record) (map[string]bool, []slog.Attr) {
	recordKeys := make(map[string]bool, r.NumAttrs())
	recordAttrs := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		a.Key = h.group + a.Key
		recordKeys[a.Key] = true
		recordAttrs = append(recordAttrs, a)
		return true
	})
	caller := make([]slog.Attr, 0, len(h.derived)+len(recordAttrs))
	own := make(map[string]bool, len(h.derived)+len(recordAttrs))
	for _, a := range h.derived {
		if !recordKeys[a.Key] {
			caller = append(caller, a)
			own[a.Key] = true
		}
	}
	caller = append(caller, recordAttrs...)
	for k := range recordKeys {
		own[k] = true
	}
	return own, caller
}

// storedValue is attrValue for an entry the tailer's ring and the sink's
// buffer keep after the call returns. A string is clipped and copied, so the
// entry neither grows with the caller's input nor keeps the caller's memory.
func storedValue(a slog.Attr) any {
	v := attrValue(a)
	if s, ok := v.(string); ok {
		return logstream.Clip(s, logstream.MaxValueBytes)
	}
	return v
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

// WithAttrs returns a new EnrichHandler with pre-added attributes. Groups
// flatten, so an attribute added under group g is stored as "g.key".
func (h *EnrichHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	derived := make([]slog.Attr, len(h.derived), len(h.derived)+len(attrs))
	copy(derived, h.derived)
	for _, a := range attrs {
		a.Key = h.group + a.Key
		derived = append(derived, a)
	}
	return &EnrichHandler{inner: h.inner, state: h.state, derived: derived, group: h.group}
}

// WithGroup returns a new EnrichHandler whose later attributes are keyed
// under name.
func (h *EnrichHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &EnrichHandler{inner: h.inner, state: h.state, derived: h.derived, group: h.group + name + "."}
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
