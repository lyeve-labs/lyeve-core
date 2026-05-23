package middleware_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// testSink is an in-memory CaptureSink for testing.
type testSink struct {
	mu      sync.Mutex
	entries []observability.CaptureEntry
}

func (s *testSink) Capture(entry observability.CaptureEntry) error {
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	s.mu.Unlock()
	return nil
}

func (s *testSink) Last() *observability.CaptureEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil
	}
	return &s.entries[len(s.entries)-1]
}

func (s *testSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func TestRequestCapture_CapturesSuccessfulResponse(t *testing.T) {
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/items", strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	rec.Flush()

	if sink.Count() != 1 {
		t.Fatalf("expected 1 captured entry, got %d", sink.Count())
	}

	entry := sink.Last()
	if entry.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", entry.Method)
	}
	if entry.URL != "/api/items" {
		t.Errorf("url = %q, want /api/items", entry.URL)
	}
	if entry.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", entry.StatusCode)
	}
	// TenantID is empty without the TenantHeader middleware being present.
	// Verified in the integration test with the full pipeline.
	if !strings.Contains(string(entry.ResponseBody), `"ok":true`) {
		t.Errorf("response body missing expected content: %s", entry.ResponseBody)
	}
	if entry.Duration <= 0 {
		t.Errorf("duration %v should be > 0", entry.Duration)
	}
	if entry.TTL != 24*time.Hour {
		t.Errorf("ttl = %v, want 24h", entry.TTL)
	}
}

func TestRequestCapture_CapturesErrorResponse(t *testing.T) {
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/broken", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if sink.Count() != 1 {
		t.Fatalf("expected 1 captured entry, got %d", sink.Count())
	}
	entry := sink.Last()
	if entry.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", entry.StatusCode)
	}
}

func TestRequestCapture_NoSinkIsNoop(t *testing.T) {
	// When sink is nil, middleware should pass through without capture.
	h := middleware.RequestCapture(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestRequestCapture_RedactsSensitiveHeaders(t *testing.T) {
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=xyz; HttpOnly; Secure")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Cookie", "session=abc123")
	req.Header.Set("X-Api-Key", "key-secret-here")
	req.Header.Set("Accept", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	entry := sink.Last()
	for name, vals := range entry.RequestHeaders {
		if name == "Authorization" || name == "Cookie" || name == "X-Api-Key" {
			for _, v := range vals {
				if v != "[REDACTED]" {
					t.Errorf("header %q should be redacted, got %q", name, v)
				}
			}
		}
	}
	// Response headers: Set-Cookie must be redacted.
	if sc := entry.ResponseHeaders.Get("Set-Cookie"); sc != "[REDACTED]" {
		t.Errorf("Set-Cookie response header should be [REDACTED], got %q", sc)
	}
}

func TestRequestCapture_CapturesResponseHeaders(t *testing.T) {
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "hello")
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/create", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	entry := sink.Last()
	if entry.ResponseHeaders.Get("X-Custom") != "hello" {
		t.Errorf("missing X-Custom header in captured response")
	}
}

func TestRequestCapture_CapturesRequestBody(t *testing.T) {
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler should still be able to read the body (we replaced it).
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo", string(body))
		w.WriteHeader(http.StatusOK)
	}))

	payload := `{"user":"alice","action":"login"}`
	req := httptest.NewRequest(http.MethodPost, "/auth", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("X-Echo") != payload {
		t.Errorf("handler couldn't read body: echo=%q", rec.Header().Get("X-Echo"))
	}
	entry := sink.Last()
	if string(entry.RequestBody) != payload {
		t.Errorf("request body = %q, want %q", entry.RequestBody, payload)
	}
}

func TestRequestCapture_WithoutTenantHeader(t *testing.T) {
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/public", nil)
	// No X-Tenant-Id header.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	entry := sink.Last()
	if entry.TenantID != "" {
		t.Errorf("tenant_id = %q, want empty", entry.TenantID)
	}
}

func TestRequestCapture_TenantSlotResolved(t *testing.T) {
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate TenantHeader resolving the tenant partway down the chain,
		// after the outer structuredLogger installed the slot.
		if slot := core.TenantSlotFrom(r.Context()); slot != nil {
			slot.Set("tenant_acme")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, _ := core.WithTenantSlot(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	entry := sink.Last()
	if entry.TenantID != "tenant_acme" {
		t.Errorf("tenant_id = %q, want tenant_acme", entry.TenantID)
	}
}

func TestRequestCapture_MultipleRequests(t *testing.T) {
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	if sink.Count() != 5 {
		t.Errorf("expected 5 entries, got %d", sink.Count())
	}
}

func TestRequestCapture_RequestBodyTruncated(t *testing.T) {
	// Verify: handler still reads the full request body, but the captured
	// copy is truncated at captureMaxBody with Truncated=true.
	capBodyLen := 1<<20 + 256 // > 1 MiB
	payload := strings.Repeat("x", capBodyLen)

	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler read failed: %v", err)
		}
		// Handler must see the FULL body, not the captured prefix.
		if len(body) != capBodyLen {
			t.Fatalf("handler saw %d bytes, want %d", len(body), capBodyLen)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/big", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	entry := sink.Last()
	if entry == nil {
		t.Fatal("no capture entry")
	}
	if !entry.Truncated {
		t.Error("expected Truncated=true for oversized request body")
	}
	if want := 1 << 20; len(entry.RequestBody) != want {
		t.Errorf("captured request body len = %d, want %d", len(entry.RequestBody), want)
	}
	// Captured prefix must be the first captureMaxBody bytes.
	if string(entry.RequestBody) != payload[:1<<20] {
		t.Error("captured request body is not the correct prefix")
	}
}

func TestRequestCapture_ResponseBodyTruncated(t *testing.T) {
	// Verify: the client receives the full response, but the captured copy
	// is truncated at captureMaxBody with Truncated=true.
	sink := &testSink{}
	h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write more than captureMaxBody bytes to the response.
		chunk := strings.Repeat("y", 64*1024) // 64 KiB
		for i := 0; i < 20; i++ {             // 1.25 MiB total
			w.Write([]byte(chunk))
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/big-response", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Client must receive the full response body.
	if rec.Body.Len() != 64*1024*20 {
		t.Fatalf("client response len = %d, want %d", rec.Body.Len(), 64*1024*20)
	}

	entry := sink.Last()
	if entry == nil {
		t.Fatal("no capture entry")
	}
	if !entry.Truncated {
		t.Error("expected Truncated=true for oversized response body")
	}
	if len(entry.ResponseBody) > 1<<20 {
		t.Errorf("captured response body len = %d, must be <= 1 MiB", len(entry.ResponseBody))
	}
	// Captured prefix should be all 'y' bytes.
	for _, b := range entry.ResponseBody {
		if b != 'y' {
			t.Error("captured response body corrupted")
			break
		}
	}
}

// TestRequestCapture_SelfLimiting_NoMaxBodySize verifies defense-in-depth:
// RequestCapture is self-limiting even when NO upstream MaxBodySize is
// wired. A body >> captureMaxBody should be truncated in the captured
// entry but the handler still sees the full payload.
func TestRequestCapture_SelfLimiting_NoMaxBodySize(t *testing.T) {
	sink := &testSink{}

	// No MaxBodySize upstream: pure RequestCapture only.
	handler := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler: read body failed: %v", err)
		}
		// Echo the full body back so we can verify completeness.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))

	// 5 MiB body: well above captureMaxBody (1 MiB) and no MaxBodySize guard.
	payloadSize := 5 << 20
	payload := strings.Repeat("Z", payloadSize)
	req := httptest.NewRequest(http.MethodPost, "/capture", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Handler must have received and echoed the FULL body.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.Len() != payloadSize {
		t.Fatalf("handler received incomplete body: got %d bytes, want %d", rec.Body.Len(), payloadSize)
	}
	if rec.Body.String() != payload {
		t.Fatal("handler body content mismatch - MultiReader stitch broken")
	}

	// Captured entry must be truncated at captureMaxBody.
	entry := sink.Last()
	if entry == nil {
		t.Fatal("no capture entry")
	}
	if !entry.Truncated {
		t.Fatal("expected Truncated=true for body exceeding capture limit")
	}
	if len(entry.RequestBody) > 1<<20 {
		t.Fatalf("captured request body size (%d) exceeds captureMaxBody (1 MiB)", len(entry.RequestBody))
	}
	if len(entry.ResponseBody) > 1<<20 {
		t.Fatalf("captured response body size (%d) exceeds captureMaxBody (1 MiB)", len(entry.ResponseBody))
	}
}

// A credential route's bodies never reach the sink: the sink redacts by field
// name, and a password or a token shown once under a name it does not know
// would be stored for any tenant admin to read. These are the engine's own
// credential routes. A route the engine does not serve withholds its bodies
// by its own declaration.
func TestRequestCapture_WithholdsCredentialBodies(t *testing.T) {
	tests := []struct {
		path     string
		withheld bool
	}{
		{"/api/admin/auth/login", true},
		{"/api/admin/auth/webauthn/register/finish", true},
		{"/api/admin/setup", true},
		{"/api/admin/users/6f1c/password", true},
		{"/api/v1/auth/token", true},
		{"/api/admin/config", true},
		{"/api/admin/plugins/email/config", true},
		{"/api/admin/plugins/email/config/reset", true},
		{"/api/admin/admin-tokens", true},
		{"/api/admin/admin-tokens/6f1c/rotate", true},
		{"/api/admin/plugins", false},
		{"/api/admin/content/posts", false},
		{"/api/v1/content/posts", false},
		{"/api/admin/schemas/password", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			sink := &testSink{}
			h := middleware.RequestCapture(sink)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"raw_key":"lyv_secret"}`))
			}))
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"name":"k"}`))
			h.ServeHTTP(httptest.NewRecorder(), req)

			entry := sink.Last()
			if entry == nil {
				t.Fatal("nothing captured; the request itself must still be recorded")
			}
			if entry.StatusCode != http.StatusCreated || entry.URL != tc.path {
				t.Errorf("entry = %s %d, want %s 201", entry.URL, entry.StatusCode, tc.path)
			}
			gotBodies := len(entry.RequestBody) > 0 || len(entry.ResponseBody) > 0
			if gotBodies == tc.withheld {
				t.Errorf("bodies captured = %v, want %v (request %q, response %q)",
					gotBodies, !tc.withheld, entry.RequestBody, entry.ResponseBody)
			}
		})
	}
}

// A route's own declaration withholds its bodies as the engine's credential
// roots do, and neither test can release what the other withholds.
func TestRequestCaptureWithholding_DeclaredRouteKeepsItsBodiesOut(t *testing.T) {
	declared := func(r *http.Request) bool { return r.URL.Path == "/api/admin/widgets/secret" }
	tests := []struct {
		name     string
		path     string
		test     func(*http.Request) bool
		withheld bool
	}{
		{"declared sensitive", "/api/admin/widgets/secret", declared, true},
		{"sibling not declared", "/api/admin/widgets", declared, false},
		{"credential root with no declaration", "/api/admin/admin-tokens", declared, true},
		{"no declaration test at all", "/api/admin/widgets/secret", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &testSink{}
			h := middleware.RequestCaptureWithholding(sink, tc.test)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"secret":"shown-once"}`))
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"code":"123456"}`)))

			entry := sink.Last()
			if entry == nil {
				t.Fatal("nothing captured; the request itself must still be recorded")
			}
			if entry.StatusCode != http.StatusCreated || entry.URL != tc.path {
				t.Errorf("entry = %s %d, want %s 201", entry.URL, entry.StatusCode, tc.path)
			}
			gotBodies := len(entry.RequestBody) > 0 || len(entry.ResponseBody) > 0
			if gotBodies == tc.withheld {
				t.Errorf("bodies captured = %v, want %v", gotBodies, !tc.withheld)
			}
		})
	}
}
