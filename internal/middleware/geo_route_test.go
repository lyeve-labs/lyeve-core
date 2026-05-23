package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGeoRoute_SetsInstanceRegionHeader(t *testing.T) {
	handler := GeoRoute("us-east-1")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "us-east-1", rec.Header().Get(GeoRouteHeader))
}

func TestGeoRoute_NoSourceHeaderServesLocally(t *testing.T) {
	called := false
	handler := GeoRoute("us-east-1")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.True(t, called)
	assert.Equal(t, "us-east-1", rec.Header().Get(GeoRouteHeader))
}

func TestGeoRoute_EmptyInstanceRegion(t *testing.T) {
	called := false
	handler := GeoRoute("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.True(t, called)
	assert.Empty(t, rec.Header().Get(GeoRouteHeader))
}
