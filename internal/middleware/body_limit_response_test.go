package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A declared Content-Length over the limit is refused before the body is read,
// so a caller who declares 10 GiB and sends a kilobyte gets an answer rather
// than a connection held open until the read timeout. The 413 uses the JSON
// envelope every other response uses: {error, code, request_id}.

func bodyLimited(maxBytes int64) http.Handler {
	reached := false
	h := MaxBodySize(maxBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = r.Body.Read(make([]byte, 1))
		w.WriteHeader(http.StatusOK)
	}))
	_ = reached
	return h
}

func postWithDeclaredLength(t *testing.T, h http.Handler, declared int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(body))
	r.ContentLength = declared
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// A huge declared length with almost nothing behind it is refused at once.
func TestMaxBodySize_RefusesADeclaredLengthBeforeReadingTheBody(t *testing.T) {
	rec := postWithDeclaredLength(t, bodyLimited(1<<20), 10<<30, "tiny")

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code,
		"a body that could never fit must be refused, not waited for")
	assert.Equal(t, "close", rec.Header().Get("Connection"),
		"the caller is mid-send; draining a body already refused means reading all of it")
}

func TestMaxBodySize_TheRefusalIsTheJSONEnvelope(t *testing.T) {
	rec := postWithDeclaredLength(t, bodyLimited(1<<20), 10<<30, "tiny")

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body),
		"answered %q, which is not the envelope", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	assert.NotEmpty(t, body["error"])
	assert.Contains(t, body, "code")
	assert.Contains(t, body, "request_id", "a refusal must be correlatable with the log")
}

// A body within the limit is untouched, declared or not.
func TestMaxBodySize_LetsAnOrdinaryBodyThrough(t *testing.T) {
	rec := postWithDeclaredLength(t, bodyLimited(1<<20), 4, "okay")

	assert.Equal(t, http.StatusOK, rec.Code)
}

// A chunked body declares no length at all, so the reader is still what bounds
// it: the header check is an addition, not a replacement.
func TestMaxBodySize_StillBoundsABodyThatDeclaresNoLength(t *testing.T) {
	h := MaxBodySize(8)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64)
		if _, err := r.Body.Read(buf); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(strings.Repeat("A", 64)))
	r.ContentLength = -1 // chunked
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// A limit of zero is off, and must not start refusing on the header either.
func TestMaxBodySize_ZeroIsStillDisabled(t *testing.T) {
	rec := postWithDeclaredLength(t, bodyLimited(0), 10<<30, "tiny")

	assert.Equal(t, http.StatusOK, rec.Code)
}

// ContentLengthLimit's own early check answers with the JSON envelope too.
func TestContentLengthLimit_RefusalIsTheJSONEnvelope(t *testing.T) {
	h := ContentLengthLimit(16)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := postWithDeclaredLength(t, h, 4096, "over the soft limit")

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body),
		"answered %q, which is not the envelope", rec.Body.String())
	assert.NotEmpty(t, body["error"])
}
