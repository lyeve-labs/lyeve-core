package middleware

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// multipartBody builds an upload whose payload is size bytes.
func multipartBody(t *testing.T, size int) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", "payload.bin")
	require.NoError(t, err)
	_, err = part.Write(bytes.Repeat([]byte("a"), size))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return &buf, w.FormDataContentType()
}

func TestJSONContentLengthLimit_RejectsOversizedJSON(t *testing.T) {
	const maxBytes = 1024

	handler := JSONContentLengthLimit(maxBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", maxBytes+1)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// An upload larger than the JSON cap has to reach the handler intact: the
// global MaxBodySize limit is what bounds it, not the 1 MiB JSON guard.
func TestJSONContentLengthLimit_PassesOversizedMultipart(t *testing.T) {
	const maxBytes = 1024
	const payload = maxBytes * 4

	var got int
	handler := JSONContentLengthLimit(maxBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		got = len(body)
		w.WriteHeader(http.StatusOK)
	}))

	body, contentType := multipartBody(t, payload)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Greater(t, got, payload, "the whole multipart body must reach the handler")
}

func TestJSONContentLengthLimit_ZeroDisablesTheGuard(t *testing.T) {
	handler := JSONContentLengthLimit(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", 1<<20)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}
