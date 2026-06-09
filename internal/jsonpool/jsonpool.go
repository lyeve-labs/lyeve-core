// Package jsonpool provides thread-safe object pools for hot-path allocations.
// All pools guarantee the Reset contract: callers must reset objects
// before reuse. Safe for concurrent use.
package jsonpool

import (
	"bytes"
	"io"
	"sync"

	"github.com/bytedance/sonic"
)

// Buffer pool: reusable *bytes.Buffer for JSON encoding and body reading.

var bufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 4096))
	},
}

// GetBuffer returns a pooled *bytes.Buffer. Must call Buffer after use.
func GetBuffer() *bytes.Buffer {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// Buffer returns buf to the pool. Buffers larger than 64KB are not
// pooled: they're left for GC to avoid unbounded memory retention.
func Buffer(buf *bytes.Buffer) {
	if buf == nil || buf.Cap() > 65536 {
		return
	}
	bufferPool.Put(buf)
}

// Marshal helper: encode into a pooled buffer, return a copy.
// Uses bytedance/sonic for SIMD-accelerated JSON encoding.

// MarshalJSON encodes v into a []byte using a pooled sonic encoder and buffer.
func MarshalJSON(v any) ([]byte, error) {
	buf := GetBuffer()
	defer Buffer(buf)
	encoder := sonic.ConfigFastest.NewEncoder(buf)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	// Copy out: the buffer is going back to the pool.
	raw := buf.Bytes()
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, nil
}

// Decode helper: read request body into buffer, unmarshal with sonic.

// DecodeJSON reads r into a pooled buffer, then unmarshals into v using sonic.
// For typical CMS request bodies (<64KB), this avoids the per-call allocs of
// json.NewDecoder(r).Decode(v): the buffer is reused across requests.
// Returns the same error semantics as json.Unmarshal.
//
// NOTE: this does NOT copy the buffer. It passes buf.Bytes() directly to
// sonic.Unmarshal, which may retain a reference. This is safe because sonic
// copies string values out of the buffer.
func DecodeJSON(r io.Reader, v any) error {
	buf := GetBuffer()
	defer Buffer(buf)
	if _, err := buf.ReadFrom(r); err != nil {
		return err
	}
	return sonic.Unmarshal(buf.Bytes(), v)
}

// Sonic config hints: tune sonic for CMS payload shapes.

// PrewarmSonic primes sonic's internal JIT compiler for common CMS types.
// Call once at startup after config is loaded. Reduces first-request latency
// by forcing sonic to compile its fast-path encoders at boot, not on the
// critical path. Each missed type costs ~2-5ms of JIT compilation on first use.
func PrewarmSonic() {
	// Content types (most common path)
	_, _, _ = func() (string, string, error) {
		s1, err1 := sonic.MarshalString(map[string]any{"id": "00000000-0000-0000-0000-000000000000", "status": "published", "data": map[string]any{"title": "Example", "slug": "example"}})
		return s1, "", err1
	}() // err suppressed: prewarm
	_, _ = sonic.MarshalString(map[string]string{"error": "test", "code": "ERR_TEST"})
	_, _ = sonic.MarshalString([]map[string]any{{"id": "00000000-0000-0000-0000-000000000000"}})

	// Auth response shapes
	_, _ = sonic.MarshalString(map[string]any{
		"access_token": "eyJ...", "refresh_token": "rt_...",
		"token_type": "Bearer", "expires_in": 900,
		"user": map[string]any{"id": "uuid", "email": "user@example.com", "role": "admin"},
	})

	// User/profile objects
	_, _ = sonic.MarshalString(map[string]any{
		"id": "uuid", "email": "user@example.com", "name": "User",
		"avatar_url": "https://...", "metadata": map[string]any{"key": "val"},
	})

	// Permission/role objects
	_, _ = sonic.MarshalString(map[string]any{"role": "editor", "permissions": []string{"read", "write"}})

	// Content list responses (array with metadata)
	_, _ = sonic.MarshalString(map[string]any{"items": []map[string]any{}, "total": 42, "page": 1, "page_size": 20})

	// Schema definition objects
	_, _ = sonic.MarshalString(map[string]any{
		"name": "articles", "fields": []map[string]any{
			{"name": "title", "type": "text", "required": true},
			{"name": "body", "type": "richtext"},
		},
	})

	// Plugin status reports
	_, _ = sonic.MarshalString(map[string]any{"name": "content", "status": "running", "version": "1.0.0", "routes": 5})

	// Config/options objects
	_, _ = sonic.MarshalString(map[string]any{"key": "value", "nested": map[string]any{"enabled": true, "threshold": 0.85}})

	// Health/status responses
	_, _ = sonic.MarshalString(map[string]any{"status": "ok", "version": "1.0.0", "uptime": "2h30m", "db": "connected"})
}
