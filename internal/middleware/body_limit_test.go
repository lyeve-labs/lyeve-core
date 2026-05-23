package middleware_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// MaxBodySize: functional tests

func TestMaxBodySize_Zero_DisablesLimit(t *testing.T) {
	handler := mw.MaxBodySize(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should be able to read the body without error.
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))

	body := strings.NewReader(strings.Repeat("x", 1024))
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestMaxBodySize_UnderLimit(t *testing.T) {
	handler := mw.MaxBodySize(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))

	body := strings.NewReader(strings.Repeat("x", 512))
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestMaxBodySize_OverLimit(t *testing.T) {
	handler := mw.MaxBodySize(256)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			// MaxBytesReader returns http.MaxBytesError -> server sends 413.
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	body := strings.NewReader(strings.Repeat("x", 1024))
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

// ContentLengthLimit: functional tests

func TestContentLengthLimit_Zero_DisablesLimit(t *testing.T) {
	handler := mw.ContentLengthLimit(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := strings.NewReader(strings.Repeat("x", 1024))
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.ContentLength = 1024
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestContentLengthLimit_UnderLimit(t *testing.T) {
	handler := mw.ContentLengthLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := strings.NewReader(strings.Repeat("x", 512))
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.ContentLength = 512
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestContentLengthLimit_OverLimit(t *testing.T) {
	handler := mw.ContentLengthLimit(256)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := strings.NewReader(strings.Repeat("x", 1024))
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.ContentLength = 1024
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestContentLengthLimit_NoContentLength_PassesThrough(t *testing.T) {
	handler := mw.ContentLengthLimit(256)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// httptest.NewRequest with nil body sets ContentLength = -1 (unknown).
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestContentLengthLimit_ExactLimit(t *testing.T) {
	handler := mw.ContentLengthLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := strings.NewReader(strings.Repeat("x", 1024))
	req := httptest.NewRequest(http.MethodPost, "/test", body)
	req.ContentLength = 1024
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for exact limit, got %d", rec.Code)
	}
}

// ContentLengthLimit + MaxBodySize: nested behavior

func TestContentLengthLimit_NestedUnderMaxBodySize(t *testing.T) {
	// Simulates the production stack: global MaxBodySize(10MB) with a
	// per-group ContentLengthLimit(1MB). Uploads up to 10MB should pass
	// the ContentLengthLimit (it only checks the header, doesn't wrap body)
	// but small JSON payloads over 1MB should be rejected.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	handler := mw.MaxBodySize(10 << 20)(
		mw.ContentLengthLimit(1 << 20)(inner),
	)

	t.Run("small JSON under 1MB passes", func(t *testing.T) {
		body := bytes.NewReader(bytes.Repeat([]byte("x"), 512))
		req := httptest.NewRequest(http.MethodPost, "/test", body)
		req.ContentLength = 512
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("JSON over 1MB rejected by ContentLengthLimit", func(t *testing.T) {
		body := bytes.NewReader(bytes.Repeat([]byte("x"), 2<<20))
		req := httptest.NewRequest(http.MethodPost, "/test", body)
		req.ContentLength = 2 << 20 // 2 MiB
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413, got %d", rec.Code)
		}
	})

	t.Run("upload with no Content-Length passes ContentLengthLimit", func(t *testing.T) {
		// When Content-Length is unknown (-1), ContentLengthLimit passes it
		// through. The global MaxBodySize still provides the hard backstop.
		req := httptest.NewRequest(http.MethodPost, "/upload", nil)
		// ContentLength is -1 when nil body -> passes ContentLengthLimit.
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
	})
}

// MaxBodySizeFor: per-route ceilings

func TestMaxBodySizeFor_DeclaredRouteAcceptsMoreThanTheGlobalLimit(t *testing.T) {
	overrides := map[string]int64{mw.BodyLimitKey("POST", "/api/admin/imports"): 4096}
	handler := mw.MaxBodySizeFor(256, overrides)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strconv.Itoa(len(n))))
	}))

	cases := []struct {
		name   string
		method string
		path   string
		size   int
		want   int
	}{
		{"declared route takes the larger body", "POST", "/api/admin/imports", 2048, http.StatusOK},
		{"trailing slash resolves to the same route", "POST", "/api/admin/imports/", 2048, http.StatusOK},
		{"declared route still has a ceiling", "POST", "/api/admin/imports", 8192, http.StatusRequestEntityTooLarge},
		{"another path keeps the global limit", "POST", "/api/admin/content", 2048, http.StatusRequestEntityTooLarge},
		{"another method keeps the global limit", "PUT", "/api/admin/imports", 2048, http.StatusRequestEntityTooLarge},
		{"a sibling path is not a prefix match", "POST", "/api/admin/imports/validate", 2048, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(bytes.Repeat([]byte("x"), tc.size)))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s %s with %d bytes: got %d, want %d", tc.method, tc.path, tc.size, rec.Code, tc.want)
			}
		})
	}
}

// An override below the global limit does not narrow the route: the global
// limit is the floor, so a plugin cannot use the declaration to weaken a
// ceiling the operator configured.
func TestMaxBodySizeFor_OverrideNeverNarrowsTheGlobalLimit(t *testing.T) {
	overrides := map[string]int64{mw.BodyLimitKey("POST", "/api/admin/imports"): 16}
	handler := mw.MaxBodySizeFor(1024, overrides)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/imports",
		bytes.NewReader(bytes.Repeat([]byte("x"), 512)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// A declared Content-Length above the ceiling is refused before the body is
// read, so a caller that announces a large upload gets an answer instead of a
// connection that hangs until the read timeout.
func TestMaxBodySizeFor_RefusesOnContentLengthAlone(t *testing.T) {
	handler := mw.MaxBodySizeFor(256, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/admin/imports", strings.NewReader(""))
	req.ContentLength = 1 << 30
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}
