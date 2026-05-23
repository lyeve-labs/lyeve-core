package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPathSanitize(t *testing.T) {
	t.Run("passes clean path", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("rejects null byte in path", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))
		// httptest.NewRequest URL parsing rejects control chars, so we
		// build the URL manually: simulates a decoded %00 in the path.
		req := &http.Request{
			Method:     http.MethodGet,
			URL:        &url.URL{Path: "/api/v1/content/foo\x00bar"},
			RequestURI: "/api/v1/content/foo%00bar",
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("rejects null byte at path boundary", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))
		req := &http.Request{
			Method:     http.MethodGet,
			URL:        &url.URL{Path: "/api/v1/content/known-schema/\x00"},
			RequestURI: "/api/v1/content/known-schema/%00",
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("rejects control character \\r in path", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))
		req := &http.Request{
			Method:     http.MethodGet,
			URL:        &url.URL{Path: "/api/v1/content/\r"},
			RequestURI: "/api/v1/content/%0d",
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("rejects DEL character (0x7f) in path", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))
		req := &http.Request{
			Method:     http.MethodGet,
			URL:        &url.URL{Path: "/api/v1/content/ab\x7fc"},
			RequestURI: "/api/v1/content/ab%7fc",
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("rejects fullwidth A in path", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))
		// U+FF21: visually identical to ASCII 'A'.
		req := &http.Request{
			Method:     http.MethodGet,
			URL:        &url.URL{Path: "/api/v1/content/\uff21"},
			RequestURI: "/api/v1/content/%ef%bc%a1",
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("rejects fullwidth characters used for schema name bypass", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be called")
		}))
		// Fullwidth "FOO" - U+FF26 U+FF2F U+FF2F.
		req := &http.Request{
			Method:     http.MethodGet,
			URL:        &url.URL{Path: "/api/v1/content/\uff26\uff2f\uff2f"},
			RequestURI: "/api/v1/content/%ef%bc%a6%ef%bc%af%ef%bc%af",
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("passes normal Unicode (non-fullwidth)", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/r%C3%A9sum%C3%A9", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("passes empty path", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("passes query string without interfering", func(t *testing.T) {
		mw := PathSanitize()
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts?limit=10&offset=20", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestContainsNullOrControl(t *testing.T) {
	t.Run("empty string", func(t *testing.T) {
		assert.False(t, ContainsNullOrControl(""))
	})
	t.Run("normal ASCII", func(t *testing.T) {
		assert.False(t, ContainsNullOrControl("hello world"))
	})
	t.Run("null byte", func(t *testing.T) {
		assert.True(t, ContainsNullOrControl("\x00"))
	})
	t.Run("control chars", func(t *testing.T) {
		assert.True(t, ContainsNullOrControl("\x01"))
		assert.True(t, ContainsNullOrControl("\x1f"))
		assert.True(t, ContainsNullOrControl("\x7f")) // DEL
	})
	t.Run("space is not control", func(t *testing.T) {
		assert.False(t, ContainsNullOrControl(" ")) // 0x20, printable
	})
	t.Run("tab is control", func(t *testing.T) {
		assert.True(t, ContainsNullOrControl("\t")) // 0x09
	})
}

func TestContainsFullwidth(t *testing.T) {
	t.Run("empty string", func(t *testing.T) {
		assert.False(t, containsFullwidth(""))
	})
	t.Run("normal ASCII", func(t *testing.T) {
		assert.False(t, containsFullwidth("hello world"))
	})
	t.Run("fullwidth A", func(t *testing.T) {
		assert.True(t, containsFullwidth("\uff21")) // U+FF21
	})
	t.Run("fullwidth range boundary", func(t *testing.T) {
		assert.False(t, containsFullwidth("\uff00")) // just below range
		assert.True(t, containsFullwidth("\uff01"))  // start of range (fullwidth !)
		assert.True(t, containsFullwidth("\uff5e"))  // end of range (fullwidth ~)
		assert.False(t, containsFullwidth("\uff5f")) // just above range
	})
	t.Run("normal CJK is not fullwidth", func(t *testing.T) {
		assert.False(t, containsFullwidth("日本語"))
	})
}

func TestFullwidthToASCII(t *testing.T) {
	t.Run("fullwidth A to ASCII A", func(t *testing.T) {
		assert.Equal(t, "A", fullwidthToASCII("\uff21"))
	})
	t.Run("fullwidth FOO to ASCII FOO", func(t *testing.T) {
		assert.Equal(t, "FOO", fullwidthToASCII("\uff26\uff2f\uff2f"))
	})
	t.Run("normal text unchanged", func(t *testing.T) {
		assert.Equal(t, "hello", fullwidthToASCII("hello"))
	})
	t.Run("mixed text", func(t *testing.T) {
		assert.Equal(t, "ABC", fullwidthToASCII("A\uff22C"))
	})
}

func TestSanitizePathParam(t *testing.T) {
	t.Run("clean param", func(t *testing.T) {
		assert.False(t, sanitizePathParam("posts"))
	})
	t.Run("null byte in param", func(t *testing.T) {
		assert.True(t, sanitizePathParam("foo\x00bar"))
	})
	t.Run("fullwidth in param", func(t *testing.T) {
		assert.True(t, sanitizePathParam("\uff26\uff2f\uff2f"))
	})
	t.Run("control char in param", func(t *testing.T) {
		assert.True(t, sanitizePathParam("\x01"))
	})
}
