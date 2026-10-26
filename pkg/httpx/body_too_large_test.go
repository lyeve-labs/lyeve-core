package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A plugin that reads a body itself answers an over-limit body the way the
// engine's own limit does: 413 in the error envelope, naming the limit, with
// the connection closed so the rest of the upload is not drained.
func TestBodyTooLarge_AnswersTheEnginesRefusal(t *testing.T) {
	rec := httptest.NewRecorder()
	BodyTooLarge(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", nil), 1024)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "close", rec.Header().Get("Connection"))
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "Request body exceeds the 1024 byte limit.", body["error"])
	assert.Contains(t, body, "request_id")
}
