package core

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestHost_PrefersTheVouchedHost(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "engine.internal:3001"
	assert.Equal(t, "engine.internal:3001", RequestHost(r))

	r = r.WithContext(WithRequestHost(r.Context(), "acme.example.com"))
	assert.Equal(t, "acme.example.com", RequestHost(r))
}
