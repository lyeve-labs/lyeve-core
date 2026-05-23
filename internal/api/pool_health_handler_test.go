package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPoolHealthHandler_NilProvider(t *testing.T) {
	handler := poolHealthHandler(nil)

	req := httptest.NewRequest(http.MethodGet, "/pool/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
	assert.Equal(t, true, body["healthy"])
	assert.Contains(t, body["message"], "not configured")
}

// fakePoolHealthProvider returns a fixed snapshot for handler wiring tests.
type fakePoolHealthProvider struct {
	snap observability.PoolHealthSnapshot
}

func (f fakePoolHealthProvider) PoolHealth(context.Context) observability.PoolHealthSnapshot {
	return f.snap
}

func TestPoolHealthHandler_WithProvider(t *testing.T) {
	provider := fakePoolHealthProvider{snap: observability.PoolHealthSnapshot{
		Engine:    "postgres",
		Healthy:   true,
		OpenConns: 5,
		InUse:     2,
	}}
	handler := poolHealthHandler(provider)

	req := httptest.NewRequest(http.MethodGet, "/pool/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var body observability.PoolHealthSnapshot
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
	assert.Equal(t, "postgres", body.Engine)
	assert.True(t, body.Healthy)
	assert.Equal(t, 5, body.OpenConns)
	assert.Equal(t, 2, body.InUse)
}
