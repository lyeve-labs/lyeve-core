package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Readiness reports the real schema count, because a deployment gate keys on
// it and a false zero is worse than no field.
func TestReadyHandler_SchemaCount(t *testing.T) {
	t.Run("reports what the cache holds", func(t *testing.T) {
		rec := httptest.NewRecorder()
		readyHandlerFn(&fakeDB{engine: "postgres"}, func() int { return 42 })(
			rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got, ok := body["schema_count"]; !ok || got.(float64) != 42 {
			t.Fatalf("schema_count = %v (present %v), want 42", got, ok)
		}
	})

	t.Run("omits the field when nothing counts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		readyHandlerFn(&fakeDB{engine: "postgres"}, nil)(
			rec, httptest.NewRequest(http.MethodGet, "/ready", nil))

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := body["schema_count"]; ok {
			t.Fatal("a count with no counter behind it must be absent, not zero")
		}
	})
}
