package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// A plugin derives its logger once with logger.With and logs through it for
// the life of the process. The stored entry and the live tail are built by the
// enrich handler, so the fields a derived logger carries have to reach both,
// or an operator searching the stored log for a flow id finds none of the
// lines stdout shows with it.
func TestEnrichHandler_DerivedAttrsReachSinkAndTail(t *testing.T) {
	prev := observability.GlobalTailer()
	t.Cleanup(func() { observability.SetGlobalTailer(prev) })

	for _, tc := range []struct {
		name   string
		derive func(*slog.Logger) *slog.Logger
		ctx    context.Context
		want   map[string]any
	}{
		{
			name:   "with",
			derive: func(l *slog.Logger) *slog.Logger { return l.With("component", "iprep", "flow_id", "f-1") },
			ctx:    context.Background(),
			want:   map[string]any{"component": "iprep", "flow_id": "f-1", "status": "ok"},
		},
		{
			name:   "nested with",
			derive: func(l *slog.Logger) *slog.Logger { return l.With("component", "iprep").With("job_id", "j-1") },
			ctx:    context.Background(),
			want:   map[string]any{"component": "iprep", "job_id": "j-1", "status": "ok"},
		},
		{
			name: "group flattens as group.key",
			derive: func(l *slog.Logger) *slog.Logger {
				return l.With("component", "iprep").WithGroup("job").With("id", "j-1")
			},
			ctx:  context.Background(),
			want: map[string]any{"component": "iprep", "job.id": "j-1", "job.status": "ok"},
		},
		{
			name:   "the record wins over a derived value",
			derive: func(l *slog.Logger) *slog.Logger { return l.With("status", "derived") },
			ctx:    context.Background(),
			want:   map[string]any{"status": "ok"},
		},
		{
			name:   "the context tenant and plugin win over derived values",
			derive: func(l *slog.Logger) *slog.Logger { return l.With("tenant_id", "globex", "plugin", "other") },
			ctx:    logging.WithPlugin(core.WithTenantID(context.Background(), "acme"), "widgets"),
			want:   map[string]any{"tenant_id": "acme", "plugin": "widgets", "status": "ok"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tailer := observability.NewLogTailer()
			observability.SetGlobalTailer(tailer)
			var console bytes.Buffer
			h := logging.NewEnrichHandler(slog.NewJSONHandler(&console, nil), nil)
			sink := &captureSink{}
			h.SetSink(sink)

			tc.derive(h.Logger()).InfoContext(tc.ctx, "derived", "status", "ok")

			stored := sink.snapshot()
			require.Len(t, stored, 1)
			tail := tailer.Recent(observability.TailFilter{}, 0)
			require.Len(t, tail, 1)
			line := consoleKeys(t, console.Bytes())
			for k, v := range tc.want {
				assert.Equal(t, v, stored[0].Attrs[k], "stored entry, key %q", k)
				assert.Equal(t, v, tail[0].Attrs[k], "tail entry, key %q", k)
				assert.Equal(t, v, line[k], "console line, key %q", k)
			}
		})
	}
}
