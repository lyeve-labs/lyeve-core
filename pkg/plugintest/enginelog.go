package plugintest

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// EngineLog holds every entry the engine's log handler stored during a test,
// as a tenant's log views read it: Attrs["tenant_id"] is the tenant a record is
// filed under, and a record without one is filed under no tenant.
type EngineLog struct {
	mu      sync.Mutex
	entries []observability.LogEntry
}

// Entries returns a copy of the stored entries in the order they were logged.
func (l *EngineLog) Entries() []observability.LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]observability.LogEntry(nil), l.entries...)
}

// stampedKeys are the attributes the handler takes from the context rather
// than from what a line says.
var stampedKeys = map[string]bool{"tenant_id": true, "plugin": true, "request_id": true, "trace_id": true, "span_id": true}

// Mentioning returns the entries whose LineText contains one of words, so a
// word that is only the request's tenant matches no line.
func (l *EngineLog) Mentioning(words ...string) []observability.LogEntry {
	var out []observability.LogEntry
	for _, e := range l.Entries() {
		text := LineText(e)
		for _, w := range words {
			if strings.Contains(text, w) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// LineText renders what a line says: its message, then " key=value" for each
// attribute the line itself carries, in key order. The attributes the handler
// stamps from the context are left out.
func LineText(e observability.LogEntry) string {
	keys := make([]string, 0, len(e.Attrs))
	for k := range e.Attrs {
		if !stampedKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(e.Message)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, e.Attrs[k])
	}
	return b.String()
}

// engineLogSink is the sink the handler writes each stored entry to.
type engineLogSink struct{ log *EngineLog }

func (s engineLogSink) Write(e observability.LogEntry) {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	s.log.entries = append(s.log.entries, e)
}

func (engineLogSink) Close() error { return nil }
func (engineLogSink) Name() string { return "plugintest" }

// discardHandler drops the console copy of each record.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

// EngineLogger returns a logger backed by the engine's own log handler, and
// the log that handler stores to. The handler stamps the context's tenant and
// plugin on each record and filters by levels, which answers per tenant and
// plugin the way the install's level settings do. A nil levels keeps Info and
// above for everyone. Hand the logger to the code under test, for example as
// the host's Logger.
func EngineLogger(levels observability.LogLevelManager) (*slog.Logger, *EngineLog) {
	l := &EngineLog{}
	h := logging.NewEnrichHandler(discardHandler{}, levels)
	h.SetSink(engineLogSink{log: l})
	return slog.New(h), l
}

// CaptureEngineLog makes an EngineLogger the default logger for the rest of
// the test and returns what it stores. The default logger is process-wide, so
// a test that captures cannot run in parallel with another test that logs.
func CaptureEngineLog(t testing.TB, levels observability.LogLevelManager) *EngineLog {
	t.Helper()
	logger, l := EngineLogger(levels)
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}
