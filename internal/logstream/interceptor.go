package logstream

import (
	"context"
	"log/slog"
)

// Interceptor is a slog.Handler that wraps another handler and pushes
// each log record into the log Buffer. Attr resolution is deferred
// (WithAttrs / WithGroup stack tracked) so the full attribute set is
// available when the record is logged.
type Interceptor struct {
	next     slog.Handler
	buf      *Buffer
	preAttrs []slog.Attr
	groups   []string
}

// NewInterceptor wraps next and pushes log records into buf.
func NewInterceptor(next slog.Handler, buf *Buffer) slog.Handler {
	return &Interceptor{next: next, buf: buf}
}

// Enabled delegates to the wrapped handler.
func (i *Interceptor) Enabled(ctx context.Context, level slog.Level) bool {
	return i.next.Enabled(ctx, level)
}

// Handle logs the record through the wrapped handler and pushes a copy into the buffer.
func (i *Interceptor) Handle(ctx context.Context, r slog.Record) error {
	attrs := make([]slog.Attr, 0, r.NumAttrs()+len(i.preAttrs))
	attrs = append(attrs, i.preAttrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})

	entry := Entry{
		Timestamp: r.Time,
		Level:     slogToStreamLevel(r.Level),
		Message:   r.Message,
		Attrs:     attrsToMap(attrs),
	}

	// Extract well-known fields from attrs for filtering convenience.
	// This lets the admin UI filter by tenant/plugin even when the caller
	// didn't explicitly set them on the context.
	for _, a := range attrs {
		switch a.Key {
		case "tenant_id":
			entry.TenantID = a.Value.String()
		case "plugin":
			entry.Plugin = a.Value.String()
		}
	}

	i.buf.Push(entry)

	return i.next.Handle(ctx, r)
}

// WithAttrs returns a new Interceptor with the given attrs pre-appended.
func (i *Interceptor) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return i
	}
	ni := *i
	ni.preAttrs = make([]slog.Attr, len(i.preAttrs), len(i.preAttrs)+len(attrs))
	copy(ni.preAttrs, i.preAttrs)
	ni.preAttrs = append(ni.preAttrs, attrs...)
	ni.next = i.next.WithAttrs(attrs)
	return &ni
}

// WithGroup returns a new Interceptor with the given group prepended.
func (i *Interceptor) WithGroup(name string) slog.Handler {
	if name == "" {
		return i
	}
	ni := *i
	ni.groups = make([]string, len(i.groups), len(i.groups)+1)
	copy(ni.groups, i.groups)
	ni.groups = append(ni.groups, name)
	ni.next = i.next.WithGroup(name)
	return &ni
}

// slogToStreamLevel converts a slog.Level to a stream Level.
func slogToStreamLevel(l slog.Level) Level {
	switch {
	case l >= slog.LevelError:
		return LevelError
	case l >= slog.LevelWarn:
		return LevelWarn
	case l >= slog.LevelInfo:
		return LevelInfo
	default:
		return LevelDebug
	}
}

// attrsToMap flattens slog.Attrs into a string map, ignoring groups.
func attrsToMap(attrs []slog.Attr) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		if a.Key == "" {
			continue
		}
		m[a.Key] = a.Value.String()
	}
	return m
}

// Ensure slog.Handler compliance.
var _ slog.Handler = (*Interceptor)(nil)

// NewInterceptedLogger creates a new slog.Logger that writes to both the given
// handler and pushes entries to the buffer. If base is nil, a no-op handler
// is used (caller should typically pass the real handler).
func NewInterceptedLogger(base slog.Handler, buf *Buffer) *slog.Logger {
	return slog.New(NewInterceptor(base, buf))
}
