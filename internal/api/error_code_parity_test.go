package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// A handler that answers through httpx.ErrorCode sends what the engine's own
// handlers send through respondErrCode, byte for byte, so a handler outside
// the engine answers exactly as one inside it does.
func TestErrorCode_MatchesTheEnginesEnvelope(t *testing.T) {
	cases := []struct {
		name      string
		loc       i18n.Locale
		requestID string
		status    int
		code      i18n.Code
		params    map[string]any
	}{
		{"catalog message", i18n.LocaleEN, "req-1", http.StatusBadRequest, i18n.CodeTenantSlugInvalid, nil},
		{"another locale", i18n.LocaleFR, "req-2", http.StatusBadRequest, i18n.CodeTenantSlugInvalid, nil},
		{"interpolated", i18n.LocaleEN, "req-3", http.StatusUnprocessableEntity, i18n.CodeTenantFeatureUnknown, map[string]any{"name": `<b>"widgets"</b> & more`}},
		{"no request id", i18n.LocaleEN, "", http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil},
		{"default parameter", i18n.LocaleFR, "req-4", http.StatusUnprocessableEntity, i18n.CodePasswordTooShort, nil},
		{"code outside the catalog", i18n.LocaleEN, "req-5", http.StatusConflict, i18n.Code("WIDGET_BUSY"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/admin/anything", nil)
			ctx := i18n.WithLocale(req.Context(), tc.loc)
			if tc.requestID != "" {
				ctx = logging.WithRequestID(ctx, tc.requestID)
			}
			req = req.WithContext(ctx)

			engine := httptest.NewRecorder()
			respondErrCode(engine, req, tc.status, tc.code, tc.params)
			outside := httptest.NewRecorder()
			httpx.ErrorCode(outside, req, tc.status, string(tc.code), tc.params)

			assert.Equal(t, engine.Code, outside.Code)
			assert.Equal(t, engine.Header(), outside.Header())
			assert.Equal(t, engine.Body.String(), outside.Body.String())
		})
	}
}
