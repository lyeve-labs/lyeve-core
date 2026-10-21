package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// The free-text erasers redact every standalone match of the identifier, some
// in every tenant, so a bare word would be redacted in the records of anyone
// whose text holds it. Both routes refuse an identifier that names no subject
// before any eraser or exporter runs.
func TestDSARHandler_RefusesAnIdentifierThatNamesNoSubject(t *testing.T) {
	eraser := &recordingEraser{}
	compliance.RegisterSubjectEraser(eraser)
	handler := NewDSARHandler(&mockAuditWriter{}, nil, nil)

	send := func(fn http.HandlerFunc, path, identifier string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]string{"identifier": identifier})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		fn(rr, req)
		return rr
	}

	for _, identifier := range []string{"anna", "admin", "anna@", "@example.com", "not-an-id-1234"} {
		t.Run("erase/"+identifier, func(t *testing.T) {
			rr := send(handler.Erase, "/api/admin/gdpr/erase", identifier)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("erase %q = %d, want 422: %s", identifier, rr.Code, rr.Body.String())
			}
			for _, seen := range eraser.seen {
				if seen == identifier {
					t.Fatalf("erase %q reached the erasers", identifier)
				}
			}
		})
		t.Run("export/"+identifier, func(t *testing.T) {
			rr := send(handler.Export, "/api/admin/gdpr/export", identifier)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("export %q = %d, want 422: %s", identifier, rr.Code, rr.Body.String())
			}
		})
	}

	for _, identifier := range []string{
		"anna@example.com",
		"0b6f1c52-3f0e-4b8e-9d37-6a1f0f3c2e11",
		"203.0.113.7",
		"2001:db8::1",
	} {
		t.Run("erase/"+identifier, func(t *testing.T) {
			rr := send(handler.Erase, "/api/admin/gdpr/erase", identifier)
			if rr.Code != http.StatusOK {
				t.Fatalf("erase %q = %d, want 200: %s", identifier, rr.Code, rr.Body.String())
			}
			reached := false
			for _, seen := range eraser.seen {
				reached = reached || seen == identifier
			}
			if !reached {
				t.Fatalf("erase %q never reached the erasers", identifier)
			}
		})
	}
}
