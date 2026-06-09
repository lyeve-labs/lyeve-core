package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPaymentRequired(t *testing.T) {
	tests := []struct {
		name       string
		plugin     string
		feature    string
		upgradeURL string
	}{
		{
			name:       "standard",
			plugin:     "widgets",
			feature:    "feature-a",
			upgradeURL: "https://example.com/license",
		},
		{
			name:       "empty upgrade_url",
			plugin:     "gadgets",
			feature:    "feature-b",
			upgradeURL: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			PaymentRequired(w, tt.plugin, tt.feature, tt.upgradeURL)

			assert.Equal(t, http.StatusPaymentRequired, w.Code)
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			assert.Equal(t, tt.upgradeURL, w.Header().Get("X-Upgrade-URL"))

			var body map[string]any
			err := json.NewDecoder(w.Body).Decode(&body)
			assert.NoError(t, err)
			assert.Equal(t, "payment_required", body["error"])
			assert.Equal(t, tt.plugin, body["plugin"])
			assert.Equal(t, tt.feature, body["feature"])
			assert.Equal(t, tt.upgradeURL, body["upgrade_url"])
		})
	}
}
