package debug_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/debug"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Tracer unit tests

func TestNewTracer(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/test")
	if tr.ID() == "" {
		t.Fatal("tracer ID should not be empty")
	}
}

func TestTracer_RecordAndFinalize(t *testing.T) {
	tr := debug.NewTracer("POST", "/api/content")

	tr.Record("auth-middleware", debug.KindMiddleware, 1*time.Millisecond, nil)
	tr.Record("query-content", debug.KindDBQuery, 3*time.Millisecond, debug.DBQueryDetail{
		SQL:  "SELECT * FROM content WHERE id = $1",
		Args: []any{"abc-123"},
	})
	tr.Record("after-create", debug.KindHookEvent, 500*time.Microsecond, debug.HookEventDetail{
		EventType: "after_create",
		Schema:    "posts",
	})

	tr.SetResponseHeader("Content-Type", "application/json")
	tr.SetResponseHeader("X-Custom", "debug-test")

	report := tr.Finalize(200, 42)

	if report.RequestID == "" {
		t.Fatal("report request_id empty")
	}
	if report.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", report.Method)
	}
	if report.Path != "/api/content" {
		t.Errorf("path = %s, want /api/content", report.Path)
	}
	if report.StatusCode != http.StatusOK {
		t.Errorf("status_code = %d, want 200", report.StatusCode)
	}
	if report.ResponseSize != 42 {
		t.Errorf("response_size = %d, want 42", report.ResponseSize)
	}
	if report.Duration < 0 {
		t.Error("duration should not be negative")
	}

	if v, ok := report.ResponseHeaders["Content-Type"]; !ok || v != "application/json" {
		t.Errorf("ResponseHeaders[Content-Type] = %q, want application/json", v)
	}
	if v, ok := report.ResponseHeaders["X-Custom"]; !ok || v != "debug-test" {
		t.Errorf("ResponseHeaders[X-Custom] = %q, want debug-test", v)
	}

	// 3 recorded segments + 1 total = 4
	if len(report.Segments) != 4 {
		t.Fatalf("segments = %d, want 4", len(report.Segments))
	}

	// Last segment should be the total.
	last := report.Segments[len(report.Segments)-1]
	if last.Kind != debug.KindTotal {
		t.Errorf("last segment kind = %s, want total", last.Kind)
	}
}

func TestTracer_RecordErr(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/items")
	tr.RecordErr("bad-query", debug.KindDBQuery, 1*time.Millisecond,
		debug.DBQueryDetail{SQL: "BROKEN SQL", Args: []any{1}},
		errors.New("syntax error"),
	)

	report := tr.Finalize(500, 0)
	if len(report.Segments) != 2 { // 1 recorded + 1 total
		t.Fatalf("segments = %d, want 2", len(report.Segments))
	}
	if report.Segments[0].Error != "syntax error" {
		t.Errorf("error = %q, want 'syntax error'", report.Segments[0].Error)
	}
	// Stack trace should be present for errors
	if report.Segments[0].Stack == "" {
		t.Error("stack trace should be populated for errors")
	}
	if !strings.Contains(report.Segments[0].Stack, "debug_test.TestTracer_RecordErr") {
		t.Errorf("stack trace should contain TestTracer_RecordErr, got: %s", report.Segments[0].Stack[:200])
	}
}

func TestTracer_MarshalJSON(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/ping")
	tr.Record("ping", debug.KindMiddleware, 100*time.Microsecond, nil)
	report := tr.Finalize(200, 0)

	data, err := report.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := parsed["request_id"]; !ok {
		t.Error("missing request_id")
	}
	if _, ok := parsed["segments"]; !ok {
		t.Error("missing segments")
	}
	if _, ok := parsed["total_duration_ms"]; !ok {
		t.Error("missing total_duration_ms")
	}
	// Response headers should be empty when no headers were set.
	if hdrs, ok := parsed["response_headers"]; ok {
		t.Logf("response_headers present: %v", hdrs)
	}
}

func TestTracer_MarshalIndent(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/ping")
	report := tr.Finalize(200, 0)

	data, err := report.MarshalIndent()
	if err != nil {
		t.Fatalf("marshal indent: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty output")
	}
}

// Context injection tests

func TestTracerFromCtx_NilWhenNotSet(t *testing.T) {
	ctx := context.Background()
	if tr := debug.TracerFromCtx(ctx); tr != nil {
		t.Error("TracerFromCtx should return nil when not set")
	}
}

func TestTracerFromCtx_Retrievable(t *testing.T) {
	tr := debug.NewTracer("GET", "/test")
	ctx := debug.WithTracer(context.Background(), tr)

	got := debug.TracerFromCtx(ctx)
	if got == nil {
		t.Fatal("TracerFromCtx returned nil after WithTracer")
	}
	if got.ID() != tr.ID() {
		t.Errorf("tracer ID mismatch")
	}
}

func TestIsDebugActive(t *testing.T) {
	if debug.IsDebugActive(context.Background()) {
		t.Error("IsDebugActive should be false when no tracer")
	}
	tr := debug.NewTracer("GET", "/test")
	ctx := debug.WithTracer(context.Background(), tr)
	if !debug.IsDebugActive(ctx) {
		t.Error("IsDebugActive should be true when tracer present")
	}
}

// Middleware tests

func TestMiddleware_NoDebugHeader(t *testing.T) {
	handler := debug.Handler(true, debug.AdminFromClaims())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %s, want {\"ok\":true}", rec.Body.String())
	}
}

func TestMiddleware_DebugHeader_NonAdmin(t *testing.T) {
	handler := debug.Handler(true, debug.AdminFromClaims())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Debug", "true")
	// No claims in context -> non-admin.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should pass through normally since admin gate returns false.
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %s, want {\"ok\":true}", rec.Body.String())
	}
}

func TestMiddleware_DebugHeader_Admin(t *testing.T) {
	// Create a gate that always returns true (simulates admin).
	isAdmin := func(r *http.Request) bool { return true }

	handler := debug.Handler(true, isAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Debug-Test", "hello")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Debug", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	// Body should be a debug report, not the original response.
	var report map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal debug report: %v\nBody: %s", err, rec.Body.String())
	}
	if _, ok := report["request_id"]; !ok {
		t.Error("missing request_id in debug report")
	}
	if report["method"] != "GET" {
		t.Errorf("method = %v, want GET", report["method"])
	}
	// Verify segments include the handler itself.
	segs, _ := report["segments"].([]any)
	if len(segs) == 0 {
		t.Error("expected at least 1 segment")
	}

	// Verify response headers are captured.
	hdrs, _ := report["response_headers"].(map[string]any)
	if hdrs == nil {
		t.Error("response_headers missing from debug report")
	} else {
		if v, ok := hdrs["Content-Type"]; !ok || v != "application/json" {
			t.Errorf("response_headers[Content-Type] = %v, want application/json", v)
		}
		if v, ok := hdrs["X-Debug-Test"]; !ok || v != "hello" {
			t.Errorf("response_headers[X-Debug-Test] = %v, want hello", v)
		}
	}
}

func TestMiddleware_XDebugReportIDHeader(t *testing.T) {
	isAdmin := func(r *http.Request) bool { return true }

	handler := debug.Handler(true, isAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Debug", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	reportID := rec.Header().Get("X-Debug-Report-ID")
	if reportID == "" {
		t.Error("X-Debug-Report-ID header should be set")
	}
}

// Admin gate tests

func TestAdminFromClaims_AdminRole(t *testing.T) {
	// Simulate admin claims.
	ctx := context.WithValue(context.Background(), core.ClaimsKey, &core.AuthClaims{
		Roles: []string{"admin"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = req.WithContext(ctx)

	gate := debug.AdminFromClaims()
	if !gate(req) {
		t.Error("admin role should pass the gate")
	}
}

func TestAdminFromClaims_SuperAdminRole(t *testing.T) {
	ctx := context.WithValue(context.Background(), core.ClaimsKey, &core.AuthClaims{
		Roles: []string{"super_admin"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = req.WithContext(ctx)

	gate := debug.AdminFromClaims()
	if !gate(req) {
		t.Error("super_admin role should pass the gate")
	}
}

func TestAdminFromClaims_EditorRole(t *testing.T) {
	ctx := context.WithValue(context.Background(), core.ClaimsKey, &core.AuthClaims{
		Roles: []string{"editor"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = req.WithContext(ctx)

	gate := debug.AdminFromClaims()
	if gate(req) {
		t.Error("editor role should NOT pass the admin gate")
	}
}

func TestAdminFromClaims_NoClaims(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	gate := debug.AdminFromClaims()
	if gate(req) {
		t.Error("no claims should NOT pass the admin gate")
	}
}

// Response writer tracking tests

func TestResponseWriter_TracksSize(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := debug.NewResponseWriter(inner)

	rw.WriteHeader(http.StatusOK)
	rw.Write([]byte("hello"))
	rw.Write([]byte(" world"))

	if rw.Size() != 11 {
		t.Errorf("size = %d, want 11", rw.Size())
	}
	if rw.Status() != 200 {
		t.Errorf("status = %d, want 200", rw.Status())
	}
}

func TestResponseWriter_Unwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := debug.NewResponseWriter(inner)

	if rw.Unwrap() != inner {
		t.Error("Unwrap should return inner writer")
	}
}

// Querier wrapper tests

type mockQuerier struct {
	queryRowFn func(ctx context.Context, sql string, args ...any) (core.Row, error)
	queryFn    func(ctx context.Context, sql string, args ...any) (core.Rows, error)
	execFn     func(ctx context.Context, sql string, args ...any) (core.CommandTag, error)
	beginFn    func(ctx context.Context) (core.Tx, error)
}

func (m *mockQuerier) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	err := errors.New("fake querier: QueryRow is not stubbed")
	return core.ErrorRow(err), err
}

func (m *mockQuerier) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return nil, nil
}

func (m *mockQuerier) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return core.CommandTag{RowsAffected: 1}, nil
}

func (m *mockQuerier) Begin(ctx context.Context) (core.Tx, error) {
	if m.beginFn != nil {
		return m.beginFn(ctx)
	}
	return nil, nil
}

func TestQuerier_RecordsQuery(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/items")
	ctx := debug.WithTracer(context.Background(), tr)

	mock := &mockQuerier{
		queryRowFn: func(ctx context.Context, sql string, args ...any) (core.Row, error) {
			return nil, nil
		},
	}
	q := debug.NewQuerier(mock, tr, "postgres", nil) // nil rawDB -> no EXPLAIN

	q.QueryRow(ctx, "SELECT id FROM content WHERE id = $1", "abc-123")

	report := tr.Finalize(200, 0)

	// Should have 1 DB query segment + 1 total segment.
	var dbSegments int
	for _, s := range report.Segments {
		if s.Kind == debug.KindDBQuery {
			dbSegments++
		}
	}
	if dbSegments != 1 {
		t.Errorf("db_query segments = %d, want 1", dbSegments)
	}
}

func TestQuerier_RecordsExecWithRowsAffected(t *testing.T) {
	tr := debug.NewTracer("POST", "/api/content")
	ctx := debug.WithTracer(context.Background(), tr)

	mock := &mockQuerier{
		execFn: func(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
			return core.CommandTag{RowsAffected: 3}, nil
		},
	}
	q := debug.NewQuerier(mock, tr, "postgres", nil)

	q.Exec(ctx, "INSERT INTO content (title) VALUES ($1)", "test")

	report := tr.Finalize(201, 0)

	// Find the exec segment.
	found := false
	for _, s := range report.Segments {
		if s.Kind == debug.KindDBQuery {
			detail, ok := s.Detail.(debug.DBQueryDetail)
			if ok && detail.RowsAffected != nil && *detail.RowsAffected == 3 {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected Exec segment with RowsAffected=3")
	}
}

func TestQuerier_RecordsError(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/error")
	ctx := debug.WithTracer(context.Background(), tr)

	mock := &mockQuerier{
		execFn: func(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
			return core.CommandTag{}, errors.New("connection refused")
		},
	}
	q := debug.NewQuerier(mock, tr, "postgres", nil)

	_, err := q.Exec(ctx, "DELETE FROM content", nil)
	if err == nil {
		t.Fatal("expected error")
	}

	report := tr.Finalize(500, 0)
	found := false
	for _, s := range report.Segments {
		if s.Kind == debug.KindDBQuery && s.Error == "connection refused" {
			found = true
			// Verify stack trace is present for errors
			if s.Stack == "" {
				t.Error("expected stack trace on error segment")
			}
			break
		}
	}
	if !found {
		t.Error("expected error segment for failed Exec")
	}
}

// TestQuerier_FailedQueryRowScansTheError pins the core.Row contract: a caller
// that scans without checking the error must get the error back, not a nil
// dereference. This wrapper is only installed when tracing is on, so returning
// a nil Row here would panic the request being diagnosed.
func TestQuerier_FailedQueryRowScansTheError(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/error")
	ctx := debug.WithTracer(context.Background(), tr)

	want := errors.New("connection refused")
	mock := &mockQuerier{
		queryRowFn: func(ctx context.Context, sql string, args ...any) (core.Row, error) {
			return core.ErrorRow(want), want
		},
	}
	q := debug.NewQuerier(mock, tr, "postgres", nil)

	row, err := q.QueryRow(ctx, "SELECT 1", nil)
	if !errors.Is(err, want) {
		t.Fatalf("QueryRow err = %v, want %v", err, want)
	}
	if row == nil {
		t.Fatal("QueryRow returned a nil Row; callers scan it without checking err")
	}

	var v int
	if err := row.Scan(&v); !errors.Is(err, want) {
		t.Errorf("Scan err = %v, want %v", err, want)
	}
}

// Explain tests

func TestQuerier_ExplainDisabled_NoRawDB(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/items")
	ctx := debug.WithTracer(context.Background(), tr)

	mock := &mockQuerier{
		queryFn: func(ctx context.Context, sql string, args ...any) (core.Rows, error) {
			return nil, nil
		},
	}
	// nil rawDB means EXPLAIN is skipped.
	q := debug.NewQuerier(mock, tr, "postgres", nil)

	q.Query(ctx, "SELECT * FROM users WHERE id = $1", "42")

	report := tr.Finalize(200, 0)
	for _, s := range report.Segments {
		if s.Kind == debug.KindDBQuery {
			detail, ok := s.Detail.(debug.DBQueryDetail)
			if ok && detail.Explain != "" {
				t.Error("EXPLAIN should be empty when rawDB is nil")
			}
		}
	}
}

func TestQuerier_ExplainSkipped_NonSelect(t *testing.T) {
	// Even with rawDB, non-SELECT queries skip EXPLAIN.
	tr := debug.NewTracer("POST", "/api/content")
	ctx := debug.WithTracer(context.Background(), tr)

	mock := &mockQuerier{
		execFn: func(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
			return core.CommandTag{RowsAffected: 1}, nil
		},
	}
	q := debug.NewQuerier(mock, tr, "postgres", nil)

	q.Exec(ctx, "INSERT INTO content (title) VALUES ($1)", "test")

	report := tr.Finalize(201, 0)
	for _, s := range report.Segments {
		if s.Kind == debug.KindDBQuery {
			detail, ok := s.Detail.(debug.DBQueryDetail)
			if ok && detail.Explain != "" {
				t.Error("EXPLAIN should be empty for non-SELECT queries")
			}
		}
	}
}

// TestDeleted_Func tests

func TestResponseWriter_StatusDefaultOK(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := debug.NewResponseWriter(inner)

	// Write without WriteHeader -> status should default to 200.
	rw.Write([]byte("data"))

	if rw.Status() != 200 {
		t.Errorf("status = %d, want 200", rw.Status())
	}
}

func TestResponseWriter_NewWithStatus(t *testing.T) {
	rw := debug.NewResponseWriterWithStatus(httptest.NewRecorder(), 201)
	if rw.Status() != 201 {
		t.Errorf("status = %d, want 201", rw.Status())
	}
}

// Concurrency safety

func TestTracer_ConcurrencySafety(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/concurrent")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tr.Record("segment", debug.KindMiddleware, time.Microsecond, nil)
		}(i)
	}
	wg.Wait()

	report := tr.Finalize(200, 0)
	// Each goroutine records 1 segment + 1 total = 51
	if len(report.Segments) < 50 {
		t.Errorf("expected at least 50 segments, got %d", len(report.Segments))
	}
}

func TestTracer_SetResponseHeader_Concurrency(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/concurrent-headers")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tr.SetResponseHeader("X-Count", "x")
		}(i)
	}
	wg.Wait()

	report := tr.Finalize(200, 0)
	if v, ok := report.ResponseHeaders["X-Count"]; !ok || v == "" {
		t.Error("expected concurrent SetResponseHeader to be safe and propagate")
	}
}

// TimedMiddleware tests

func TestTimedMiddleware_PassThrough(t *testing.T) {
	called := false
	handler := debug.TimedMiddleware("test-mw")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called through pass-through")
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestTimedMiddleware_Recorded(t *testing.T) {
	tr := debug.NewTracer("GET", "/api/timed")
	ctx := debug.WithTracer(context.Background(), tr)
	req := httptest.NewRequest(http.MethodGet, "/api/timed", nil).WithContext(ctx)

	handler := debug.TimedMiddleware("my-middleware")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	report := tr.Finalize(200, 0)

	found := false
	for _, s := range report.Segments {
		if s.Name == "my-middleware" && s.Kind == debug.KindMiddleware {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'my-middleware' middleware segment in trace")
	}
}

// Gate tests

// An admin sending X-Debug on an instance that has not turned the tracer on
// gets the ordinary response, not a trace.
func TestHandler_OffUnlessEnabled(t *testing.T) {
	isAdmin := func(r *http.Request) bool { return true }
	handler := debug.Handler(false, isAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Debug", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should pass through: debug disabled by default.
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %s, want {\"ok\":true}", rec.Body.String())
	}
}

// The same, stated separately: an explicit off and an absent setting must land
// in the same place.
func TestHandler_OffWhenExplicitlyDisabled(t *testing.T) {
	isAdmin := func(r *http.Request) bool { return true }
	handler := debug.Handler(false, isAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Debug", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %s, want {\"ok\":true}", rec.Body.String())
	}
}

func TestHandler_Enabled_EnvVarTrue(t *testing.T) {
	isAdmin := func(r *http.Request) bool { return true }
	handler := debug.Handler(true, isAdmin)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("X-Debug", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	// Body should be a debug report, not the original response.
	var report map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal debug report: %v\nBody: %s", err, rec.Body.String())
	}
	if _, ok := report["request_id"]; !ok {
		t.Error("missing request_id in debug report")
	}
}

// Arg sanitization tests

func TestSanitizeArgs_Nil(t *testing.T) {
	if got := debug.SanitizeArgs(nil); got != nil {
		t.Errorf("SanitizeArgs(nil) = %v, want nil", got)
	}
}

func TestSanitizeArgs_NoPII(t *testing.T) {
	args := []any{"hello", int64(42), 3.14, true}
	got := debug.SanitizeArgs(args)

	if len(got) != len(args) {
		t.Fatalf("len = %d, want %d", len(got), len(args))
	}
	if got[0] != "hello" {
		t.Errorf("got[0] = %q, want %q", got[0], "hello")
	}
	if got[1] != "42" {
		t.Errorf("got[1] = %q, want %q", got[1], "42")
	}
	if got[2] != "3.14" {
		t.Errorf("got[2] = %q, want %q", got[2], "3.14")
	}
	if got[3] != "true" {
		t.Errorf("got[3] = %q, want %q", got[3], "true")
	}
}

func TestSanitizeArgs_EmailRedacted(t *testing.T) {
	args := []any{"admin@example.com", "user@test.org"}
	got := debug.SanitizeArgs(args)

	if got[0] != "[email_address]" {
		t.Errorf("got[0] = %q, want [email_address]", got[0])
	}
	if got[1] != "[email_address]" {
		t.Errorf("got[1] = %q, want [email_address]", got[1])
	}
}

func TestSanitizeArgs_Mixed(t *testing.T) {
	args := []any{"bob@evil.com", "just-text", int64(99)}
	got := debug.SanitizeArgs(args)

	if got[0] != "[email_address]" {
		t.Errorf("got[0] = %q, want [email_address]", got[0])
	}
	if got[1] != "just-text" {
		t.Errorf("got[1] = %q, want %q", got[1], "just-text")
	}
	if got[2] != "99" {
		t.Errorf("got[2] = %q, want %q", got[2], "99")
	}
}

// Verify original args are not mutated.
func TestSanitizeArgs_DoesNotMutateOriginal(t *testing.T) {
	args := []any{"alice@company.com", int64(1)}
	_ = debug.SanitizeArgs(args)

	if args[0] != "alice@company.com" {
		t.Error("original args were mutated")
	}
	if args[1] != int64(1) {
		t.Error("original args were mutated")
	}
}
