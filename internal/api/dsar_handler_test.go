package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// - Test helpers --------------------------------------------

// mockAuditWriter records audit calls for assertion.
type mockAuditWriter struct {
	calls []auditCall
}

type auditCall struct {
	Action       string
	ResourceType string
	ResourceID   string
	IP           string
	UserAgent    string
	UserID       *string
}

func (m *mockAuditWriter) LogEntry(action, resourceType, resourceID, ip, userAgent string, userID *string) {
	m.calls = append(m.calls, auditCall{
		Action: action, ResourceType: resourceType, ResourceID: resourceID,
		IP: ip, UserAgent: userAgent, UserID: userID,
	})
}

var _ compliance.DSARAuditWriter = (*mockAuditWriter)(nil)

// testExporter is a mock SubjectExporter for handler tests.
type testExporter struct {
	data map[string]any
	err  error
}

func (e *testExporter) ExportSubject(ctx context.Context, identifier string) (map[string]any, error) {
	return e.data, e.err
}

// testEraser is a mock SubjectEraser for handler tests.
type testEraser struct {
	affected int64
	err      error
}

func (e *testEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	return e.affected, e.err
}

// - Export handler tests ------------------------------------

func TestDSARHandler_Export_Success(t *testing.T) {
	compliance.RegisterSubjectExporter(&testExporter{
		data: map[string]any{
			"notes": []map[string]any{
				{"id": "1", "body": "hello"},
				{"id": "2", "body": "world"},
			},
		},
	})
	defer compliance.RegisterSubjectExporter(nil)

	audit := &mockAuditWriter{}
	handler := NewDSARHandler(audit, nil, nil)

	body := `{"identifier": "user@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/export", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Export(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result compliance.SubjectExportResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if result.Identifier != "user@example.com" {
		t.Errorf("expected identifier 'user@example.com', got %q", result.Identifier)
	}
	if len(result.Plugins) == 0 {
		t.Error("expected non-empty plugins map")
	}

	if len(audit.calls) != 1 {
		t.Fatalf("expected 1 audit call, got %d", len(audit.calls))
	}
	if audit.calls[0].Action != "gdpr.export" {
		t.Errorf("expected audit action 'gdpr.export', got %q", audit.calls[0].Action)
	}
	if audit.calls[0].ResourceID != "user@example.com" {
		t.Errorf("expected audit resource_id 'user@example.com', got %q", audit.calls[0].ResourceID)
	}
}

func TestDSARHandler_Export_InvalidBody(t *testing.T) {
	handler := NewDSARHandler(nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/export", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Export(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestDSARHandler_Export_EmptyIdentifier(t *testing.T) {
	handler := NewDSARHandler(nil, nil, nil)

	body := `{"identifier": ""}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/export", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Export(rr, req)

	if rr.Code == http.StatusOK {
		t.Error("expected non-200 for empty identifier")
	}
}

// - Erase handler tests -------------------------------------

func TestDSARHandler_Erase_Success(t *testing.T) {
	compliance.RegisterSubjectEraser(&testEraser{affected: 5})
	defer compliance.RegisterSubjectEraser(nil)

	audit := &mockAuditWriter{}
	handler := NewDSARHandler(audit, nil, nil)

	body := `{"identifier": "user@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Erase(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if _, ok := result["total_rows"]; !ok {
		t.Error("expected total_rows in response")
	}

	if len(audit.calls) != 1 {
		t.Fatalf("expected 1 audit call, got %d", len(audit.calls))
	}
	if audit.calls[0].Action != "gdpr.erase" {
		t.Errorf("expected audit action 'gdpr.erase', got %q", audit.calls[0].Action)
	}
}

func TestDSARHandler_Erase_InvalidBody(t *testing.T) {
	handler := NewDSARHandler(nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Erase(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestDSARHandler_Erase_EmptyIdentifier(t *testing.T) {
	handler := NewDSARHandler(nil, nil, nil)

	body := `{"identifier": ""}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Erase(rr, req)

	if rr.Code == http.StatusOK {
		t.Error("expected non-200 for empty identifier")
	}
}

// - Error sanitization tests --------------------------------

// Eraser that returns raw driver error text must yield a sanitized
// response, not leak the error to the API consumer.
func TestDSARHandler_Erase_ErrorsSanitized(t *testing.T) {
	compliance.RegisterSubjectEraser(&testEraser{
		affected: 0,
		err:      errors.New("ERROR: relation \"sys_users\" does not exist (SQLSTATE 42P01)"),
	})
	defer compliance.ResetSubjectErasers()

	handler := NewDSARHandler(nil, nil, nil)

	body := `{"identifier": "user@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Erase(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	errorsRaw, ok := resp["errors"]
	if !ok {
		t.Fatal("expected 'errors' field in response")
	}
	errorsList, ok := errorsRaw.([]any)
	if !ok {
		t.Fatalf("expected errors to be an array, got %T", errorsRaw)
	}
	if len(errorsList) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errorsList))
	}

	errStr, ok := errorsList[0].(string)
	if !ok {
		t.Fatalf("expected error to be string, got %T", errorsList[0])
	}

	// Must be the static sanitized message, not the raw driver error.
	if errStr != "one or more erasers failed" {
		t.Errorf("error sanitization failed: got %q, want %q", errStr, "one or more erasers failed")
	}
	if strings.Contains(errStr, "SQLSTATE") || strings.Contains(errStr, "sys_users") {
		t.Errorf("raw driver error text leaked into response: %q", errStr)
	}
}

// Multiple failing erasers with different error texts all collapse
// to a single sanitized message.
func TestDSARHandler_Erase_MultipleFailuresSanitized(t *testing.T) {
	compliance.RegisterSubjectEraser(&testEraser{
		affected: 0,
		err:      errors.New("connection refused: 127.0.0.1:5432"),
	})
	compliance.RegisterSubjectEraser(&testEraser{
		affected: 0,
		err:      errors.New("table \"sys_example\" not found"),
	})
	defer compliance.ResetSubjectErasers()

	handler := NewDSARHandler(nil, nil, nil)

	body := `{"identifier": "user@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Erase(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	errorsRaw, ok := resp["errors"]
	if !ok {
		t.Fatal("expected 'errors' field in response")
	}
	errorsList, ok := errorsRaw.([]any)
	if !ok {
		t.Fatalf("expected errors to be an array, got %T", errorsRaw)
	}

	if len(errorsList) != 1 {
		t.Fatalf("expected 1 error entry, got %d: %v", len(errorsList), errorsList)
	}
	errStr, _ := errorsList[0].(string)
	if errStr != "one or more erasers failed" {
		t.Errorf("error sanitization failed: got %q, want %q", errStr, "one or more erasers failed")
	}
}

// The audit writer is supplied by a plugin and can be absent, so the gap has
// to be visible in the log, and the log must not become the place the
// subject's identifier ends up instead.
func TestDSARHandler_Export_WithoutAuditWriterReportsTheGap(t *testing.T) {
	compliance.RegisterSubjectExporter(&testExporter{data: map[string]any{"notes": []map[string]any{}}})
	defer compliance.RegisterSubjectExporter(nil)

	cap := captureAccessLog(t)
	handler := NewDSARHandler(nil, nil, nil)

	body := `{"identifier": "subject@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/export", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.Export(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var warned bool
	for _, line := range strings.Split(cap.buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["level"] != "WARN" || !strings.Contains(toString(rec["msg"]), "no audit trail") {
			continue
		}
		warned = true
		if rec["action"] != "gdpr.export" {
			t.Errorf("warning action = %v, want gdpr.export", rec["action"])
		}
		if strings.Contains(line, "subject@example.com") {
			t.Errorf("the warning carries the subject identifier: %s", line)
		}
	}
	if !warned {
		t.Error("an export ran with no audit writer and said nothing about it")
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}
