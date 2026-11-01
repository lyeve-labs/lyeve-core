package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// engineLog points the default logger at the engine's enrich handler over a
// buffer for one test, so a line carries what the engine adds from the
// context: the request id and the tenant.
func engineLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(logging.NewEnrichHandler(slog.NewJSONHandler(&buf, nil), nil).Logger())
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// storeErrorRequest is a tenant's request as a handler sees it: the request
// id under both keys the engine's middleware sets, and a matched route.
func storeErrorRequest(requestID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/admin/error-tracking/codes", nil)
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{"/api/admin/error-tracking/codes"}
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, chimw.RequestIDKey, requestID)
	ctx = logging.WithRequestID(ctx, requestID)
	ctx = core.WithTenantID(ctx, "acme")
	return r.WithContext(ctx)
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		out = append(out, rec)
	}
	return out
}

func TestStoreError_LogsAFailedStoreCallOnceWithItsRequestID(t *testing.T) {
	buf := engineLog(t)
	const driverText = "driver: connection reset by peer, row acme-secret"
	err := fmt.Errorf("register error code: %w", errors.New(driverText))

	w := httptest.NewRecorder()
	StoreError(w, storeErrorRequest("rid-503"), err, "failed to register error code")

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotContains(t, w.Body.String(), driverText, "driver text reached the response")
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "rid-503", body["request_id"])

	lines := logLines(t, buf)
	require.Len(t, lines, 1, "one warn line per store failure answered with a 5xx")
	line := lines[0]
	assert.Equal(t, "WARN", line["level"])
	assert.Equal(t, "rid-503", line["request_id"], "the line joins the access line by its request id")
	assert.Equal(t, float64(http.StatusServiceUnavailable), line["status"])
	assert.Equal(t, http.MethodPost, line["method"])
	assert.Equal(t, "/api/admin/error-tracking/codes", line["route"])
	assert.Equal(t, err.Error(), line["error"])
	assert.NotContains(t, line, "tenant_id",
		"driver text can carry column values, so the line is never filed under a tenant its admins read")
}

func TestStoreError_LogsNothingForACallerOutcome(t *testing.T) {
	buf := engineLog(t)
	for _, tc := range []struct {
		err    error
		status int
	}{
		{core.ErrNotFound, http.StatusNotFound},
		{fmt.Errorf("create: %w", core.ErrConflict), http.StatusConflict},
		{fmt.Errorf("save: %w", core.ErrValidation), http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		StoreError(w, storeErrorRequest("rid-4xx"), tc.err, "failed")
		assert.Equal(t, tc.status, w.Code, tc.err.Error())
	}
	assert.Empty(t, buf.String(), "a 4xx is the caller's outcome, not a failure to diagnose")
}
