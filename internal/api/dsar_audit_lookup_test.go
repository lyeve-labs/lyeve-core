package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

func postDSAR(t *testing.T, handle http.HandlerFunc, path, identifier string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"identifier": identifier})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handle(rr, req)
	return rr
}

func lookUpSubject(t *testing.T, h *DSARHandler, identifier string) []string {
	t.Helper()
	rr := postDSAR(t, h.SubjectDigests, "/api/admin/gdpr/subject-digests", identifier)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp SubjectDigestsResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp.Digests
}

func unkeyedSubjectDigest(identifier string) string {
	sum := sha256.Sum256([]byte(identifier))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// A request typed with other capitals than the stored address is the same
// subject, and the auditor who later looks the subject up from the stored
// account finds its entries. A guess at the address hashed without the
// instance key matches none of them, and entries written before the digest
// was keyed are still found.
func TestDSARHandler_AuditEntryIsFoundFromTheStoredAddress(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		local := "Jane.Doe-" + uuid.NewString()[:8]
		created, err := users.Create(ctx, local+"@Example.com", "!hash!", []string{"editor"}, "t1")
		require.NoError(t, err)
		t.Cleanup(func() { _ = users.Delete(ctx, created.ID) })
		account, err := users.GetByID(ctx, created.ID)
		require.NoError(t, err)
		stored := account.Email
		typed := strings.ToUpper(local) + "@EXAMPLE.com"
		require.NotEqual(t, stored, typed)

		compliance.RegisterSubjectEraser(&testEraser{affected: 1})
		t.Cleanup(compliance.ResetSubjectErasers)
		compliance.RegisterSubjectExporter(&testExporter{data: map[string]any{"notes": []map[string]any{{"id": "1"}}}})
		t.Cleanup(compliance.ResetSubjectExporters)
		audit := &mockAuditWriter{}
		h := NewDSARHandler(audit, nil, nil)

		require.Equal(t, http.StatusOK, postDSAR(t, h.Export, "/api/admin/gdpr/export", typed).Code)
		require.Equal(t, http.StatusOK, postDSAR(t, h.Erase, "/api/admin/gdpr/erase", typed).Code)
		require.Len(t, audit.calls, 2)

		found := lookUpSubject(t, h, stored)
		for _, c := range audit.calls {
			assert.Contains(t, found, c.ResourceID, "%s: the entry is not found from the stored address", c.Action)
			for _, guess := range []string{unkeyedSubjectDigest(stored), unkeyedSubjectDigest(typed)} {
				assert.NotEqual(t, guess, c.ResourceID, "%s: a guess hashed without the key matches the entry", c.Action)
			}
		}

		// Before the digest was keyed, a DSAR entry hashed the identifier as
		// the request typed it, and a refused sign-in hashed the stored
		// address. Both stay findable from the identifier the request named.
		found = lookUpSubject(t, h, typed)
		assert.Contains(t, found, unkeyedSubjectDigest(typed), "an earlier DSAR entry is not found")
		assert.Contains(t, found, unkeyedSubjectDigest(stored), "an earlier sign-in entry is not found")
	})
}

func TestDSARHandler_SubjectDigestsRefusesWhatNamesNoSubject(t *testing.T) {
	h := NewDSARHandler(nil, nil, nil)
	assert.Equal(t, http.StatusUnprocessableEntity, postDSAR(t, h.SubjectDigests, "/api/admin/gdpr/subject-digests", "Jane").Code)
	assert.Equal(t, http.StatusBadRequest, postDSAR(t, h.SubjectDigests, "/api/admin/gdpr/subject-digests", "").Code)
}
