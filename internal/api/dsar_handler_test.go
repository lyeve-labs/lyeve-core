package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// signOutFailingAccount is an account eraser whose sign-out fails.
type signOutFailingAccount struct{}

func (signOutFailingAccount) ErasesAccount()     {}
func (signOutFailingAccount) EraserName() string { return "account" }
func (signOutFailingAccount) EndSessions(context.Context, string) error {
	return errors.New("connection refused: 127.0.0.1:5432")
}
func (signOutFailingAccount) EraseSubject(context.Context, string) (int64, error) { return 1, nil }

// A failed sign-out is a database failure with nothing erased. It answered
// 207, which reads as a partial erasure. It is now the retryable 503 the
// hold-check refusal answers.
func TestDSARHandler_Erase_SignOutFailedIsRetryable(t *testing.T) {
	compliance.RegisterSubjectEraser(signOutFailingAccount{})
	compliance.RegisterSubjectEraser(&testEraser{affected: 3})
	defer compliance.ResetSubjectErasers()

	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewBufferString(`{"identifier": "user@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	NewDSARHandler(nil, nil, nil).Erase(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "not attempted") {
		t.Errorf("the answer does not say the erasure was not attempted: %s", body)
	}
	if strings.Contains(body, "127.0.0.1") {
		t.Errorf("raw driver error text leaked into response: %s", body)
	}
}

// slowEraser holds the fan-out for a fixed time, or until its context ends,
// and records whether it ran to the end.
type slowEraser struct {
	started chan struct{}
	release chan struct{}
	hold    time.Duration
	mu      sync.Mutex
	ran     bool
}

func (e *slowEraser) EraseSubject(ctx context.Context, _ string) (int64, error) {
	if e.started != nil {
		close(e.started)
	}
	if e.release != nil {
		<-e.release
	}
	select {
	case <-time.After(e.hold):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	e.mu.Lock()
	e.ran = true
	e.mu.Unlock()
	return 1, nil
}

func (e *slowEraser) finished() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ran
}

// An erasure that outlasts the listener's write timeout still finished, but
// the caller read EOF with no status, so it could not tell a complete erasure
// from a partial one.
func TestDSARHandler_Erase_OutlastsTheWriteTimeout(t *testing.T) {
	compliance.RegisterSubjectEraser(&slowEraser{hold: 400 * time.Millisecond})
	defer compliance.ResetSubjectErasers()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(NewDSARHandler(nil, nil, nil).Erase))
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	defer srv.Close()

	res, err := srv.Client().Post(srv.URL, "application/json", strings.NewReader(`{"identifier": "user@example.com"}`))
	if err != nil {
		t.Fatalf("the erasure answered nothing: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var resp EraseResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		t.Fatalf("the answer has no readable body: %v", err)
	}
	if !resp.Complete || resp.TotalRows != 1 {
		t.Errorf("answer = %+v, want a complete erasure of one row", resp)
	}
}

// A caller or proxy that gives up canceled the request's context, and the
// erasure stopped partway. The run now finishes, and its outcome is in the
// log and the audit trail although nobody read the answer.
func TestDSARHandler_Erase_ClientDisconnectDoesNotStopIt(t *testing.T) {
	first := &slowEraser{started: make(chan struct{}), release: make(chan struct{}), hold: 300 * time.Millisecond}
	second := &slowEraser{}
	compliance.RegisterSubjectEraser(first)
	compliance.RegisterSubjectEraser(second)
	defer compliance.ResetSubjectErasers()

	cap := captureAccessLog(t)
	audit := &mockAuditWriter{}
	handler := NewDSARHandler(audit, nil, nil)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		handler.Erase(w, r)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(`{"identifier": "user@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	go func() {
		<-first.started
		cancel()
		close(first.release)
	}()
	if res, err := srv.Client().Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatal("the request was answered before the client went away")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the erasure did not finish")
	}
	if !first.finished() || !second.finished() {
		t.Fatalf("the disconnect stopped the erasure: first ran %v, second ran %v", first.finished(), second.finished())
	}
	if len(audit.calls) != 1 || audit.calls[0].Action != "gdpr.erase" {
		t.Errorf("audit calls = %+v, want one gdpr.erase", audit.calls)
	}

	cap.mu.Lock()
	raw := cap.buf.String()
	cap.mu.Unlock()
	var outcome map[string]any
	for _, line := range strings.Split(raw, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "subject_erasure_outcome" {
			outcome = rec
		}
	}
	if outcome == nil {
		t.Fatal("no log line records the outcome of an erasure whose answer was not read")
	}
	if outcome["status"] != float64(http.StatusOK) || outcome["complete"] != true {
		t.Errorf("outcome = %v, want status 200 and complete", outcome)
	}
	if outcome["subject_ref"] != compliance.SubjectRef("user@example.com") {
		t.Errorf("outcome subject_ref = %v, want the erasure's subject reference", outcome["subject_ref"])
	}
	if strings.Contains(raw, "user@example.com") {
		t.Error("the log carries the subject identifier")
	}
}

// unresolvableAccount cannot read the subject's accounts.
type unresolvableAccount struct{}

func (unresolvableAccount) EraserName() string { return "account" }
func (unresolvableAccount) SubjectAccounts(context.Context, string) ([]compliance.SubjectAccount, error) {
	return nil, errors.New("connection refused: 127.0.0.1:5432")
}
func (unresolvableAccount) EraseSubject(context.Context, string) (int64, error) { return 1, nil }

// An erasure whose subject addresses cannot be read erases nothing. It
// answered 207 with every part listed as failed, which reads as a partial
// erasure. It is the retryable 503 a failed sign-out answers.
func TestDSARHandler_Erase_UnresolvedAddressesIsRetryable(t *testing.T) {
	compliance.RegisterSubjectEraser(unresolvableAccount{})
	compliance.RegisterSubjectEraser(&testEraser{affected: 3})
	defer compliance.ResetSubjectErasers()

	req := httptest.NewRequest(http.MethodPost, "/api/admin/gdpr/erase", bytes.NewBufferString(`{"identifier": "user@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	NewDSARHandler(nil, nil, nil).Erase(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "not attempted") {
		t.Errorf("the answer does not say the erasure was not attempted: %s", body)
	}
	if strings.Contains(body, "127.0.0.1") {
		t.Errorf("raw driver error text leaked into response: %s", body)
	}
}
