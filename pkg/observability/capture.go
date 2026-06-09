package observability

import (
	"net/http"
	"time"
)

// CaptureEntry is a captured HTTP request/response pair ready for storage.
// The middleware produces this. The CaptureSink persists it.
type CaptureEntry struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Method    string    `json:"method"`
	URL       string    `json:"url"`

	RequestHeaders http.Header `json:"request_headers"`
	RequestBody    []byte      `json:"request_body"`

	StatusCode      int         `json:"status_code"`
	ResponseHeaders http.Header `json:"response_headers"`
	ResponseBody    []byte      `json:"response_body"`

	Duration time.Duration `json:"duration"`

	// TenantID is the X-Tenant-ID header value, empty when not set.
	TenantID string `json:"tenant_id,omitempty"`

	// TTL is the requested lifetime for this entry. 0 means system default.
	TTL time.Duration `json:"ttl,omitempty"`

	// Truncated is true when the middleware hit the per-body capture limit
	// and the body in this entry is a prefix, not the full content.
	Truncated bool `json:"truncated,omitempty"`
}

// CaptureSink receives captured request/response entries.
// The implementation is free to drop entries under load: the middleware
// treats Capture calls as fire-and-forget (best-effort).
//
// A nil CaptureSink is valid and means "no capture in effect" (no-op).
type CaptureSink interface {
	Capture(entry CaptureEntry) error
}

// SensitiveHeaders is the canonical list of HTTP header names whose values
// must be redacted before capture or logging. Plugins and middleware use this
// list to avoid leaking credentials in stored request traces.
var SensitiveHeaders = []string{
	"Authorization",
	"Cookie",
	"Set-Cookie",
	"X-Api-Key",
	"X-Session-Token",
	"X-Csrf-Token",
}

// CaptureSinkProvider is the shape a plugin implements to supply the sink.
// It is exported so the plugin pins itself against the same symbol the engine
// asserts, rather than a private restatement of it that can drift out of
// agreement without breaking either build.
type CaptureSinkProvider interface {
	CaptureSink() CaptureSink
}
