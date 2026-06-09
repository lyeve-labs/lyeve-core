package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
)

type errorEnvelope struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

func errorCodeResponse(t *testing.T, loc i18n.Locale, requestID string, status int, code string, params map[string]any) (*httptest.ResponseRecorder, errorEnvelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/anything", nil)
	ctx := i18n.WithLocale(req.Context(), loc)
	if requestID != "" {
		ctx = logging.WithRequestID(ctx, requestID)
	}
	rec := httptest.NewRecorder()
	ErrorCode(rec, req.WithContext(ctx), status, code, params)
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	return rec, env
}

// The message is the catalog's for the caller's locale, the code is the one
// passed, and the request id rides along for the log.
func TestErrorCode_WritesTheLocalizedEnvelope(t *testing.T) {
	rec, env := errorCodeResponse(t, i18n.LocaleFR, "req-7", http.StatusBadRequest, string(i18n.CodeTenantSlugInvalid), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, i18n.CodeTenantSlugInvalid.Error(i18n.LocaleFR, nil), env.Error)
	assert.NotEqual(t, i18n.CodeTenantSlugInvalid.Error(i18n.LocaleEN, nil), env.Error, "the French message differs from the English one")
	assert.Equal(t, "TENANT_SLUG_INVALID", env.Code)
	assert.Equal(t, "req-7", env.RequestID)
}

// Parameters are interpolated into the message, and a code the catalog does
// not hold is its own message rather than a blank one.
func TestErrorCode_InterpolatesAndFallsBack(t *testing.T) {
	_, env := errorCodeResponse(t, i18n.LocaleEN, "", http.StatusUnprocessableEntity, string(i18n.CodeTenantFeatureUnknown), map[string]any{"name": "<search>"})
	assert.Equal(t, "<search> is not a plugin this instance serves.", env.Error)
	assert.Empty(t, env.RequestID)

	_, env = errorCodeResponse(t, i18n.LocaleEN, "", http.StatusConflict, "WIDGET_BUSY", nil)
	assert.Equal(t, "WIDGET_BUSY", env.Error)
	assert.Equal(t, "WIDGET_BUSY", env.Code)
}
