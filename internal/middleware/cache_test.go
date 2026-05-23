package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachePolicyBuildCacheControl(t *testing.T) {
	tests := []struct {
		name   string
		policy CachePolicy
		want   string
	}{
		{
			name:   "no directives",
			policy: CachePolicy{},
			want:   "private",
		},
		{
			name:   "public max-age only",
			policy: CachePolicy{MaxAge: 60 * time.Second, Public: true},
			want:   "public, max-age=60",
		},
		{
			name: "full public with stale",
			policy: CachePolicy{
				MaxAge:               30 * time.Second,
				StaleWhileRevalidate: 300 * time.Second,
				StaleIfError:         86400 * time.Second,
				Public:               true,
			},
			want: "public, max-age=30, stale-while-revalidate=300, stale-if-error=86400",
		},
		{
			name: "private with no-transform",
			policy: CachePolicy{
				MaxAge:      120 * time.Second,
				NoTransform: true,
			},
			want: "private, max-age=120, no-transform",
		},
		{
			name:   "no-store",
			policy: CachePolicy{NoStore: true},
			want:   "no-store",
		},
		{
			name:   "no-store with no-transform",
			policy: CachePolicy{NoStore: true, NoTransform: true},
			want:   "no-store, no-transform",
		},
		{
			name:   "no-cache",
			policy: CachePolicy{NoCache: true},
			want:   "private, no-cache",
		},
		{
			name:   "must-revalidate",
			policy: CachePolicy{MaxAge: 60 * time.Second, MustRevalidate: true},
			want:   "private, max-age=60, must-revalidate",
		},
		{
			name:   "immutable",
			policy: ImmutablePolicy,
			want:   "public, max-age=31536000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.buildCacheControl()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDefaultContentPolicy(t *testing.T) {
	assert.Equal(t, 60*time.Second, DefaultContentPolicy.MaxAge)
	assert.Equal(t, 600*time.Second, DefaultContentPolicy.StaleWhileRevalidate)
	assert.True(t, DefaultContentPolicy.Public)
}

func TestDefaultDashboardPolicy(t *testing.T) {
	assert.Equal(t, 30*time.Second, DefaultDashboardPolicy.MaxAge)
	assert.Equal(t, 300*time.Second, DefaultDashboardPolicy.StaleWhileRevalidate)
	assert.True(t, DefaultDashboardPolicy.NoTransform)
}

func TestDefaultSchemaPolicy(t *testing.T) {
	assert.Equal(t, 300*time.Second, DefaultSchemaPolicy.MaxAge)
	assert.True(t, DefaultSchemaPolicy.Public)
}

func TestCacheHeaders_2xxResponse(t *testing.T) {
	handler := cacheHeaders(CachePolicy{
		MaxAge:               60 * time.Second,
		StaleWhileRevalidate: 600 * time.Second,
		Public:               true,
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"hello": "world"})
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	// Cache-Control
	assert.Equal(t, "public, max-age=60, stale-while-revalidate=600", resp.Header.Get("Cache-Control"))

	// ETag must be present and strong
	etag := resp.Header.Get("ETag")
	assert.NotEmpty(t, etag)
	assert.True(t, strings.HasPrefix(etag, `"`))
	assert.True(t, strings.HasSuffix(etag, `"`))

	// Vary must include Authorization
	assert.Contains(t, resp.Header.Get("Vary"), "Authorization")

	// Body must be intact
	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "world", body["hello"])
}

func TestCacheHeaders_DeterministicETag(t *testing.T) {
	makeHandler := func() http.Handler {
		return cacheHeaders(CachePolicy{MaxAge: 60 * time.Second, Public: true})(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Write([]byte("hello world"))
			}),
		)
	}

	// Same content -> same ETag
	req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec1 := httptest.NewRecorder()
	makeHandler().ServeHTTP(rec1, req1)

	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec2 := httptest.NewRecorder()
	makeHandler().ServeHTTP(rec2, req2)

	assert.Equal(t, rec1.Header().Get("ETag"), rec2.Header().Get("ETag"))
}

func TestCacheHeaders_DifferentETag(t *testing.T) {
	handler := cacheHeaders(CachePolicy{MaxAge: 60 * time.Second, Public: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			// Return different content based on query
			w.Write([]byte(r.URL.Query().Get("msg")))
		}),
	)

	reqA := httptest.NewRequest(http.MethodGet, "/test?msg=alpha", nil)
	recA := httptest.NewRecorder()
	handler.ServeHTTP(recA, reqA)

	reqB := httptest.NewRequest(http.MethodGet, "/test?msg=beta", nil)
	recB := httptest.NewRecorder()
	handler.ServeHTTP(recB, reqB)

	assert.NotEqual(t, recA.Header().Get("ETag"), recB.Header().Get("ETag"))
}

func TestCacheHeaders_NoETagOnEmptyBody(t *testing.T) {
	handler := cacheHeaders(CachePolicy{MaxAge: 60 * time.Second, Public: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("ETag"))
}

func TestCacheHeaders_Non2xxResponse(t *testing.T) {
	handler := cacheHeaders(CachePolicy{MaxAge: 60 * time.Second, Public: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	// Non-2xx: should NOT have Cache-Control or ETag set by our middleware
	assert.Empty(t, resp.Header.Get("ETag"))
	assert.Empty(t, resp.Header.Get("Cache-Control"))
}

func TestCacheHeaders_LastModifiedFromXUpdatedAt(t *testing.T) {
	handler := cacheHeaders(CachePolicy{MaxAge: 60 * time.Second, Public: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Updated-At", "2025-06-01T12:00:00Z")
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	lm := rec.Header().Get("Last-Modified")
	assert.NotEmpty(t, lm)
	// Must be valid HTTP-date format
	_, err := time.Parse(time.RFC3339, lm)
	if err != nil {
		_, err = time.Parse(http.TimeFormat, lm)
	}
	assert.NoError(t, err)
}

func TestCacheHeaders_VaryMerging(t *testing.T) {
	handler := cacheHeaders(CachePolicy{MaxAge: 60 * time.Second, Public: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Vary", "Accept-Encoding")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	vary := rec.Header().Get("Vary")
	assert.Contains(t, vary, "Accept-Encoding")
	assert.Contains(t, vary, "Authorization")
}

func TestCacheHeaders_VaryKeepsEveryHandlerValue(t *testing.T) {
	handler := cacheHeaders(CachePolicy{MaxAge: 60 * time.Second, Public: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// CORS and a localized read each add their own value, as the
			// engine's middleware and content handlers do.
			w.Header().Add("Vary", "Origin")
			w.Header().Add("Vary", "Accept-Language")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, []string{"Origin, Accept-Language, Authorization"}, rec.Header().Values("Vary"))
}

func TestMergeVary(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		extra  []string
		want   string
	}{
		{name: "empty", extra: []string{"Authorization"}, want: "Authorization"},
		{name: "one value", values: []string{"Accept-Encoding"}, extra: []string{"Authorization"}, want: "Accept-Encoding, Authorization"},
		{name: "several values", values: []string{"Origin", "Accept-Language"}, extra: []string{"Authorization"}, want: "Origin, Accept-Language, Authorization"},
		{name: "comma list in one value", values: []string{"Origin, Accept-Language"}, extra: []string{"Authorization"}, want: "Origin, Accept-Language, Authorization"},
		{name: "already present", values: []string{"Authorization, Origin"}, extra: []string{"Authorization"}, want: "Authorization, Origin"},
		{name: "case insensitive dedupe keeps first spelling", values: []string{"authorization", "Origin", "origin"}, extra: []string{"Authorization"}, want: "authorization, Origin"},
		{name: "blank members dropped", values: []string{" , Origin ,, "}, extra: []string{"Authorization"}, want: "Origin, Authorization"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mergeVary(tt.values, tt.extra...))
		})
	}
}

func TestCacheHeaders_NoStore(t *testing.T) {
	handler := cacheHeaders(NoCachePolicy)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("secret"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestConditional_NoHeaders(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello", rec.Body.String())
}

func TestConditional_IfNoneMatch_Match(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"abc123"`)
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestConditional_IfNoneMatch_WeakMatch(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `W/"abc123"`)
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", `W/"abc123"`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotModified, rec.Code)
}

func TestConditional_IfNoneMatch_NoMatch(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"abc123"`)
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", `"xyz789"`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello", rec.Body.String())
}

func TestConditional_IfNoneMatch_Wildcard(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"anything"`)
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", "*")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotModified, rec.Code)
}

func TestConditional_IfNoneMatch_Multiple(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"abc123"`)
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", `"xyz", "abc123", "def"`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotModified, rec.Code)
}

func TestConditional_IfModifiedSince_Match(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Last-Modified", "Mon, 02 Jun 2025 12:00:00 GMT")
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-Modified-Since", "Tue, 03 Jun 2025 12:00:00 GMT") // newer
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotModified, rec.Code)
}

func TestConditional_IfModifiedSince_NoMatch(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Last-Modified", "Tue, 03 Jun 2025 12:00:00 GMT")
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-Modified-Since", "Mon, 02 Jun 2025 12:00:00 GMT") // older
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestConditional_Non2xx_PassesThrough(t *testing.T) {
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", `"anything"`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Non-2xx: pass through without 304
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestConditional_IfNoneMatchTakesPrecedence(t *testing.T) {
	// If-None-Match takes precedence over If-Modified-Since (RFC 7232 §3.3).
	// Even if Last-Modified is old, matching ETag should win.
	handler := conditional()(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"match"`)
			w.Header().Set("Last-Modified", "Tue, 03 Jun 2025 12:00:00 GMT") // newer than req
			w.Write([]byte("hello"))
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", `"match"`)
	req.Header.Set("If-Modified-Since", "Mon, 02 Jun 2025 12:00:00 GMT") // older: would NOT match
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// ETag match wins -> 304
	assert.Equal(t, http.StatusNotModified, rec.Code)
}

func TestCacheWithPolicy_304OnRepeatedGET(t *testing.T) {
	handler := CacheWithPolicy(CachePolicy{
		MaxAge:               60 * time.Second,
		StaleWhileRevalidate: 600 * time.Second,
		Public:               true,
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello world"))
	}))

	req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusOK, rec1.Code)

	etag := rec1.Header().Get("ETag")
	cc := rec1.Header().Get("Cache-Control")
	assert.NotEmpty(t, etag)
	assert.Contains(t, cc, "max-age=60")

	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusNotModified, rec2.Code)
}

func TestCacheWithPolicy_ContextPropagation(t *testing.T) {
	handler := CacheWithPolicy(CachePolicy{MaxAge: 30 * time.Second, Public: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Request-ID", r.Header.Get("X-Request-ID"))
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("ok"))
		}),
	)

	reqA := httptest.NewRequest(http.MethodGet, "/test", nil)
	reqA.Header.Set("X-Request-ID", "alpha")
	recA := httptest.NewRecorder()
	handler.ServeHTTP(recA, reqA)
	assert.Equal(t, "alpha", recA.Header().Get("X-Request-ID"))

	reqB := httptest.NewRequest(http.MethodGet, "/test", nil)
	reqB.Header.Set("X-Request-ID", "beta")
	recB := httptest.NewRecorder()
	handler.ServeHTTP(recB, reqB)
	assert.Equal(t, "beta", recB.Header().Get("X-Request-ID"))

	// Same body -> same ETag regardless of request headers.
	assert.Equal(t, recA.Header().Get("ETag"), recB.Header().Get("ETag"))
}

// A streaming handler pushes its write deadline out so a long response is not
// cut mid-body. It reaches the connection through http.ResponseController,
// which walks the writer chain by Unwrap. Both writers here buffer the body and
// forward Flusher, Pusher and Hijacker, so without Unwrap a handler asking for
// a deadline would get ErrNotSupported with nothing to say why.
func TestCacheWriters_ResponseControllerReachesTheConnection(t *testing.T) {
	deadlines := make(chan error, 1)
	handler := CacheWithPolicy(CachePolicy{MaxAge: time.Minute})(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			deadlines <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Without conditional request headers only etagWriter is in the chain. With
	// them conditionalWriter wraps it, so both orderings are covered.
	for _, tt := range []struct {
		name    string
		headers map[string]string
	}{
		{"etag writer alone", nil},
		{"conditional writer over etag writer", map[string]string{"If-None-Match": `"stale"`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
			require.NoError(t, err)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer res.Body.Close() //nolint:errcheck

			require.NoError(t, <-deadlines,
				"the handler must be able to extend its own write deadline through the chain")
		})
	}
}

// Unwrap must not become a way around the buffering the two writers exist to
// do: the controller has to keep using their Flush, or a flushed response
// commits to the connection while the buffered bytes are still held back.
func TestCacheWriters_FlushStillGoesThroughTheWrapper(t *testing.T) {
	handler := CacheWithPolicy(CachePolicy{MaxAge: time.Minute})(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("first"))
			require.NoError(t, http.NewResponseController(w).Flush())
			_, _ = w.Write([]byte("second"))
		}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	res, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.Equal(t, "firstsecond", string(body),
		"a flush must commit the buffered bytes, not drop them")
}

func TestEtagMatch(t *testing.T) {
	tests := []struct {
		name        string
		etag        string
		ifNoneMatch string
		want        bool
	}{
		{"exact strong", `"abc"`, `"abc"`, true},
		{"exact weak", `W/"abc"`, `W/"abc"`, true},
		{"strong matches weak", `"abc"`, `W/"abc"`, true},
		{"weak matches strong", `W/"abc"`, `"abc"`, true},
		{"wildcard", `"abc"`, "*", true},
		{"wildcard in list", `"abc"`, `"xyz", *`, true},
		{"mismatch", `"abc"`, `"xyz"`, false},
		{"empty etag", ``, `"abc"`, false},
		{"empty client header", `"abc"`, "", false},
		{"no W/ on server weak", `W/"abc"`, `W/"def"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, etagMatch(tt.etag, tt.ifNoneMatch))
		})
	}
}
