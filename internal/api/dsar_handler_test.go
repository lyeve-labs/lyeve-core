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
	if want := compliance.SubjectDigest("user@example.com"); audit.calls[0].ResourceID != want {
		t.Errorf("expected audit resource_id %q, got %q", want, audit.calls[0].ResourceID)
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
	if result["complete"] != true {
		t.Errorf("complete = %v, want true", result["complete"])
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

	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("expected 207, got %d: %s", rr.Code, rr.Body.String())
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
	assertIncompleteErasure(t, resp, errorsList)
	for _, e := range errorsList {
		errStr, _ := e.(string)
		if strings.Contains(errStr, "SQLSTATE") || strings.Contains(errStr, "sys_users") {
			t.Errorf("raw driver error text leaked into response: %q", errStr)
		}
	}
}

// assertIncompleteErasure checks that a partial erasure says so: not
// complete, the eraser named, the account not erased, and how to finish.
func assertIncompleteErasure(t *testing.T, resp map[string]any, errorsList []any) {
	t.Helper()
	if resp["complete"] != false {
		t.Errorf("complete = %v, want false", resp["complete"])
	}
	failed, _ := resp["failed"].([]any)
	if len(failed) == 0 {
		t.Errorf("failed names no eraser: %v", resp)
	}
	var joined strings.Builder
	for _, e := range errorsList {
		s, _ := e.(string)
		joined.WriteString(s + "\n")
	}
	for _, want := range []string{"not finished: ", "the account was not erased", "repeat the request"} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("errors do not say %q: %v", want, errorsList)
		}
	}
}

// Multiple failing erasers are each named, and none of their error text
// reaches the response.
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

	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("expected 207, got %d: %s", rr.Code, rr.Body.String())
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

	assertIncompleteErasure(t, resp, errorsList)
	for _, e := range errorsList {
		errStr, _ := e.(string)
		if strings.Contains(errStr, "127.0.0.1") || strings.Contains(errStr, "sys_example") {
			t.Errorf("raw driver error text leaked into response: %q", errStr)
		}
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
