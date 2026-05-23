package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exportReq(t *testing.T, body map[string]any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/export", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// An install with no tenancy wiring must refuse a cross-tenant request rather
// than answer it from one tenant. Answering narrowly is the defect: the caller
// asked for every tenant, got one, and had no way to tell.
func TestDSARExport_RefusesACrossTenantRequestItCannotAnswer(t *testing.T) {
	h := NewDSARHandler(nil, nil, nil)

	rec := httptest.NewRecorder()
	h.Export(rec, exportReq(t, map[string]any{
		"identifier":  "sam@example.com",
		"all_tenants": true,
	}))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "cross-tenant export is not available on this install")
	assert.NotContains(t, rec.Body.String(), "plugins",
		"a refusal must not also return a partial bundle")
}

// The same install answers an ordinary single-tenant export normally, so the
// refusal above is scoped to the sweep rather than breaking the endpoint.
func TestDSARExport_StillAnswersASingleTenantRequestWithNoTenancyWiring(t *testing.T) {
	h := NewDSARHandler(nil, nil, nil)

	rec := httptest.NewRecorder()
	h.Export(rec, exportReq(t, map[string]any{"identifier": "sam@example.com"}))

	assert.Equal(t, http.StatusOK, rec.Code)
	var got compliance.SubjectExportResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "tenant", got.Scope.Mode)
}

func TestDSARExport_SweepsEveryTenantWhenAsked(t *testing.T) {
	lister := func(ctx context.Context) ([]string, error) {
		return []string{"acme", "globex"}, nil
	}
	scope := func(ctx context.Context, tenant string) (context.Context, func(), error) {
		return ctx, func() {}, nil
	}
	h := NewDSARHandler(nil, lister, scope)

	rec := httptest.NewRecorder()
	h.Export(rec, exportReq(t, map[string]any{
		"identifier":  "sam@example.com",
		"all_tenants": true,
	}))

	assert.Equal(t, http.StatusOK, rec.Code)
	var got compliance.SubjectExportResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "all_tenants", got.Scope.Mode)
	assert.Equal(t, 2, got.Scope.Requested)
	assert.Equal(t, 2, got.Scope.Covered)
	assert.ElementsMatch(t, []string{"acme", "globex"}, got.Scope.Tenants)
}

// The flag has to be asked for. A super_admin carries no tenant claim, so if
// an unscoped request swept by default a forgotten header would read as a
// request for every tenant's data.
func TestDSARExport_DoesNotSweepUnlessAsked(t *testing.T) {
	swept := false
	lister := func(ctx context.Context) ([]string, error) {
		swept = true
		return []string{"acme", "globex"}, nil
	}
	scope := func(ctx context.Context, tenant string) (context.Context, func(), error) {
		return ctx, func() {}, nil
	}
	h := NewDSARHandler(nil, lister, scope)

	rec := httptest.NewRecorder()
	h.Export(rec, exportReq(t, map[string]any{"identifier": "sam@example.com"}))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, swept, "an export that did not ask for every tenant must not enumerate them")
}
