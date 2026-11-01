package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// An erasure exists to remove the subject's data, and its own audit entry kept
// the address it was asked about, in clear, for as long as the log is kept.
// The entry names the subject by a digest anyone holding the identifier can
// recompute, which proves the request happened without keeping who it was
// about.
func TestDSARHandler_AuditNamesTheSubjectByDigest(t *testing.T) {
	const identifier = "user@example.com"
	sum := sha256.Sum256([]byte(identifier))
	want := "sha256:" + hex.EncodeToString(sum[:])

	compliance.RegisterSubjectEraser(&testEraser{affected: 1})
	t.Cleanup(compliance.ResetSubjectErasers)
	compliance.RegisterSubjectExporter(&testExporter{data: map[string]any{"notes": []map[string]any{{"id": "1"}}}})
	t.Cleanup(compliance.ResetSubjectExporters)

	audit := &mockAuditWriter{}
	handler := NewDSARHandler(audit, nil, nil)
	for _, call := range []struct {
		path string
		run  func(http.ResponseWriter, *http.Request)
	}{
		{"/api/admin/gdpr/export", handler.Export},
		{"/api/admin/gdpr/erase", handler.Erase},
	} {
		req := httptest.NewRequest(http.MethodPost, call.path, bytes.NewBufferString(`{"identifier": "`+identifier+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		call.run(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", call.path, rr.Code, rr.Body.String())
		}
	}

	if len(audit.calls) != 2 {
		t.Fatalf("expected 2 audit calls, got %d", len(audit.calls))
	}
	for _, c := range audit.calls {
		if c.ResourceID != want {
			t.Errorf("%s: resource_id %q, want %q", c.Action, c.ResourceID, want)
		}
		if strings.Contains(c.ResourceID, "@") {
			t.Errorf("%s: resource_id keeps the address", c.Action)
		}
	}
}
