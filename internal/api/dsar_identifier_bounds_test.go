package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// recordingEraser reports whether the orchestration reached it, so a rejected
// identifier can be shown to stop at the handler rather than fanning out.
type recordingEraser struct {
	seen []string
}

func (e *recordingEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	e.seen = append(e.seen, identifier)
	return 0, nil
}

// An identifier longer than an address can be reaches every registered eraser
// and fails on SQL Server, which refuses a comparison value wider than the
// column: every eraser would report a truncation error, and the caller would
// see a 5xx for input that can never match a row.
func TestDSARHandler_Erase_RejectsIdentifierLongerThanAnAddress(t *testing.T) {
	eraser := &recordingEraser{}
	compliance.RegisterSubjectEraser(eraser)

	handler := NewDSARHandler(&mockAuditWriter{}, nil, nil)

	oversize := strings.Repeat("x", 321)
	body, err := json.Marshal(map[string]string{"identifier": oversize})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Erase(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("oversize identifier must be refused with 400, got %d: %s", rr.Code, rr.Body.String())
	}
	for _, seen := range eraser.seen {
		if seen == oversize {
			t.Error("a refused identifier must not reach the erasers")
		}
	}
	if strings.Contains(rr.Body.String(), oversize) {
		t.Error("the response must not echo the rejected identifier back")
	}
}

func TestDSARHandler_Export_RejectsIdentifierLongerThanAnAddress(t *testing.T) {
	handler := NewDSARHandler(&mockAuditWriter{}, nil, nil)

	body, err := json.Marshal(map[string]string{"identifier": strings.Repeat("x", 321)})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/export", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Export(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("oversize identifier must be refused with 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// The longest address RFC 5321 allows must still be accepted, so the bound
// refuses only what could never identify a subject.
func TestDSARHandler_Erase_AcceptsTheLongestAddressAllowed(t *testing.T) {
	compliance.RegisterSubjectEraser(&recordingEraser{})

	handler := NewDSARHandler(&mockAuditWriter{}, nil, nil)

	longest := strings.Repeat("a", 64) + "@" + strings.Repeat("b", 251) + ".com"
	if len(longest) != 320 {
		t.Fatalf("fixture must be exactly 320 characters, got %d", len(longest))
	}
	body, err := json.Marshal(map[string]string{"identifier": longest})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Erase(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("an address at the RFC limit must be accepted, got %d: %s", rr.Code, rr.Body.String())
	}
}
