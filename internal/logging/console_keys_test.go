package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// consoleKeys decodes one JSON line and returns its top-level keys in order,
// failing on a key the line carries twice. A strict JSON reader refuses such a
// line, and a pipeline that keeps one value per key keeps whichever it saw last.
func consoleKeys(t *testing.T, line []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)
	values := map[string]any{}
	for dec.More() {
		tok, err := dec.Token()
		require.NoError(t, err)
		key := tok.(string)
		var v any
		require.NoError(t, dec.Decode(&v))
		_, seen := values[key]
		require.False(t, seen, "the line carries %q twice: %s", key, line)
		values[key] = v
	}
	return values
}

// The request logger names request_id, trace_id and span_id on its own line,
// and the handler adds the same keys from the context. A tenant-scoped line
// also had tenant_id from two places. Each key has to reach the console once,
// with tracing on and off.
func TestEnrichHandler_ConsoleLineCarriesEachKeyOnce(t *testing.T) {
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	for _, tc := range []struct {
		name    string
		tracing bool
	}{{"tracing off", false}, {"tracing on", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := logging.NewEnrichHandler(slog.NewJSONHandler(&buf, nil), nil)

			ctx := context.WithValue(context.Background(), middleware.RequestIDKey, "req-1")
			ctx = core.WithTenantID(ctx, "acme")
			ctx = logging.WithPlugin(ctx, "widgets")
			traceID, spanID := trace.TraceID{}.String(), trace.SpanID{}.String()
			if tc.tracing {
				ctx = trace.ContextWithSpanContext(ctx, spanCtx)
				traceID, spanID = spanCtx.TraceID().String(), spanCtx.SpanID().String()
			}

			h.Logger().InfoContext(ctx, "request",
				"status", 200,
				"request_id", "req-1",
				"trace_id", traceID,
				"span_id", spanID,
			)

			got := consoleKeys(t, buf.Bytes())
			require.Equal(t, "req-1", got["request_id"])
			require.Equal(t, "acme", got["tenant_id"])
			require.Equal(t, "widgets", got["plugin"])
			require.Equal(t, traceID, got["trace_id"])
			require.Equal(t, spanID, got["span_id"])
		})
	}
}

// A caller cannot file a console line under another tenant or plugin, the
// same rule the stored entry follows.
func TestEnrichHandler_ConsoleLineKeepsTheContextTenant(t *testing.T) {
	var buf bytes.Buffer
	h := logging.NewEnrichHandler(slog.NewJSONHandler(&buf, nil), nil)
	ctx := logging.WithPlugin(core.WithTenantID(context.Background(), "acme"), "widgets")

	h.Logger().WarnContext(ctx, "suspicious", "tenant_id", "globex", "plugin", "other")

	got := consoleKeys(t, buf.Bytes())
	require.Equal(t, "acme", got["tenant_id"])
	require.Equal(t, "widgets", got["plugin"])
}
