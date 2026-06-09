// Package debug provides per-request debug tracing for plugin execution,
// middleware timing, SQL query capture, and HookBus event chains.
//
// Triggered by X-Debug: true header, gated behind admin role.
// Replaces the response body with a JSON debug report.
package debug

import (
	"encoding/json"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Kind identifies the type of debug segment.
type Kind string

const (
	KindMiddleware Kind = "middleware"
	KindPlugin     Kind = "plugin"
	KindDBQuery    Kind = "db_query"
	KindHookEvent  Kind = "hook_event"
	KindTotal      Kind = "total"
)

// Segment is a single timed operation within a request.
type Segment struct {
	Name     string  `json:"name"`
	Kind     Kind    `json:"kind"`
	Phase    string  `json:"phase,omitempty"` // "before", "after" for hooks
	Duration float64 `json:"duration_ms"`     // milliseconds
	Detail   any     `json:"detail,omitempty"`
	Error    string  `json:"error,omitempty"`
	Stack    string  `json:"stack,omitempty"` // goroutine stack when error occurred
}

// DBQueryDetail is the detail payload for KindDBQuery segments.
type DBQueryDetail struct {
	SQL          string `json:"sql"`
	Args         []any  `json:"args"`
	RowsAffected *int64 `json:"rows_affected,omitempty"`
	Explain      string `json:"explain,omitempty"` // EXPLAIN plan for SELECT queries
}

// HookEventDetail is the detail payload for KindHookEvent segments.
type HookEventDetail struct {
	EventType string  `json:"event_type"`
	Schema    string  `json:"schema"`
	Duration  float64 `json:"duration_ms,omitempty"` // hook handler execution time
}

// Report is the full debug output for a single request.
type Report struct {
	RequestID       string            `json:"request_id"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	StartTime       time.Time         `json:"start_time"`
	Duration        float64           `json:"total_duration_ms"`
	StatusCode      int               `json:"status_code"`
	ResponseSize    int64             `json:"response_size_bytes"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	Segments        []Segment         `json:"segments"`
}

// Tracer collects timing segments scoped to a single request.
// Safe for concurrent use (middleware, handlers, and DB queries may
// run in different goroutines: though typical HTTP handlers are
// single-goroutine, the tracer is mutex-protected as a safety net).
type Tracer struct {
	mu              sync.Mutex
	id              string
	method          string
	path            string
	start           time.Time
	segments        []Segment
	responseHeaders map[string]string
}

// NewTracer creates a Tracer for the given request metadata.
func NewTracer(method, path string) *Tracer {
	return &Tracer{
		id:     uuid.New().String(),
		method: method,
		path:   path,
		start:  time.Now(),
	}
}

// ID returns the tracer's unique request ID.
func (t *Tracer) ID() string { return t.id }

// SetResponseHeader records a response header for inclusion in the debug report.
// Safe for concurrent use.
func (t *Tracer) SetResponseHeader(key, value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.responseHeaders == nil {
		t.responseHeaders = make(map[string]string)
	}
	t.responseHeaders[key] = value
}

// Record appends a segment to the trace.
func (t *Tracer) Record(name string, kind Kind, dur time.Duration, detail any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.segments = append(t.segments, Segment{
		Name:     name,
		Kind:     kind,
		Duration: float64(dur.Microseconds()) / 1000.0,
		Detail:   detail,
	})
}

// RecordErr appends a segment with an error annotation and goroutine stack trace.
func (t *Tracer) RecordErr(name string, kind Kind, dur time.Duration, detail any, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	seg := Segment{
		Name:     name,
		Kind:     kind,
		Duration: float64(dur.Microseconds()) / 1000.0,
		Detail:   detail,
	}
	if err != nil {
		seg.Error = err.Error()
		// Capture goroutine stack trace (4KB buffer, truncated if larger).
		buf := make([]byte, 4096)
		n := runtime.Stack(buf, false)
		seg.Stack = string(buf[:n])
	}
	t.segments = append(t.segments, seg)
}

// Finalize builds the Report from collected segments.
func (t *Tracer) Finalize(statusCode int, responseSize int64) *Report {
	dur := time.Since(t.start)
	t.Record("request", KindTotal, dur, nil)

	t.mu.Lock()
	segs := make([]Segment, len(t.segments))
	copy(segs, t.segments)
	hdrs := make(map[string]string, len(t.responseHeaders))
	for k, v := range t.responseHeaders {
		hdrs[k] = v
	}
	t.mu.Unlock()

	return &Report{
		RequestID:       t.id,
		Method:          t.method,
		Path:            t.path,
		StartTime:       t.start,
		Duration:        float64(dur.Microseconds()) / 1000.0,
		StatusCode:      statusCode,
		ResponseSize:    responseSize,
		ResponseHeaders: hdrs,
		Segments:        segs,
	}
}

// MarshalJSON returns the JSON-encoded report without the custom MarshalJSON
// calling itself (infinite recursion). Uses a type alias to break the cycle.
func (r *Report) MarshalJSON() ([]byte, error) {
	type Alias Report
	return json.Marshal(&struct {
		*Alias
	}{
		Alias: (*Alias)(r),
	})
}

// MarshalIndent returns a pretty-printed JSON report.
func (r *Report) MarshalIndent() ([]byte, error) {
	type Alias Report
	return json.MarshalIndent(&struct {
		*Alias
	}{
		Alias: (*Alias)(r),
	}, "", "  ")
}
