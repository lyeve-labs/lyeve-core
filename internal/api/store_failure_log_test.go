package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A store failure answered with a 5xx is the one response whose cause the
// body never carries, so the cause has to reach the log under the request id
// the client was given. These cover the two answer shapes the shared helper
// does not write itself: the localized envelope with an error code, and the
// content handlers' own status mapping.

const storeFailureDriverText = "driver: connection reset by peer"

// storeFailureLog points the default logger at the engine's enrich handler
// over a buffer, so a line carries the request id and tenant the engine adds.
func storeFailureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(logging.NewEnrichHandler(slog.NewJSONHandler(&buf, nil), nil).Logger())
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// withRequestID sets the request id under both keys the engine's middleware
// sets: the one the log enrichment reads and the one the body echoes.
func withRequestID(ctx context.Context, id string) context.Context {
	ctx = context.WithValue(ctx, chimw.RequestIDKey, id)
	return logging.WithRequestID(ctx, id)
}

// linesFor returns the log lines that carry requestID.
func linesFor(t *testing.T, buf *bytes.Buffer, requestID string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		if rec["request_id"] == requestID {
			out = append(out, rec)
		}
	}
	return out
}

func assertOneStoreFailureLine(t *testing.T, buf *bytes.Buffer, requestID string) {
	t.Helper()
	lines := linesFor(t, buf, requestID)
	require.Len(t, lines, 1, "one line per store failure answered with a 5xx, joined to the access line by its request id")
	assert.Equal(t, "WARN", lines[0]["level"])
	assert.Equal(t, float64(http.StatusServiceUnavailable), lines[0]["status"])
	assert.Contains(t, lines[0]["error"], storeFailureDriverText)
	assert.NotContains(t, lines[0], "tenant_id",
		"driver text can carry column values, so the line is never filed under a tenant its admins read")
}

// failingFirstAdminStore holds no account, so setup proceeds, and fails the
// write that would create the first one.
type failingFirstAdminStore struct{ *fakeUserStore }

func (*failingFirstAdminStore) CreateFirstAdmin(context.Context, string, string, string) (*domain.User, error) {
	return nil, errors.New(storeFailureDriverText)
}

func TestSetup_StoreFailureLogsOnceWithItsRequestID(t *testing.T) {
	buf := storeFailureLog(t)

	users := &failingFirstAdminStore{fakeUserStore: &fakeUserStore{byEmail: map[string]*domain.User{}}}
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)
	h.WithSetupToken(NewSetupTokenFromEnv("operator-setup-token-0123"))
	body := `{"email":"admin@test.com","password":"CorrectHorseBatteryStaple1!","setup_token":"operator-setup-token-0123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := withRequestID(core.WithTenantID(req.Context(), "acme"), "rid-setup")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()

	h.Setup(rr, req)

	require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	want, err := json.Marshal(errResponse{
		Error:     i18n.CodeUserCreateFailed.Error(i18n.LocaleFromCtx(ctx), nil),
		Code:      string(i18n.CodeUserCreateFailed),
		RequestID: "rid-setup",
	})
	require.NoError(t, err)
	assert.JSONEq(t, string(want), rr.Body.String(), "the body keeps its error code and says nothing of the cause")
	assertOneStoreFailureLine(t, buf, "rid-setup")
}

// failingRevisions is a revision store whose database cannot answer.
type failingRevisions struct{ memRevisions }

func (*failingRevisions) ListRecordRevisions(context.Context, string, uuid.UUID) ([]core.RecordRevision, error) {
	return nil, errors.New(storeFailureDriverText)
}

func TestContentHandler_ListRevisions_StoreFailureLogsOnceWithItsRequestID(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		buf := storeFailureLog(t)
		h := *fx.content
		h.revisions = &revisionProvider{s: &failingRevisions{}}

		id := uuid.New().String()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/content/notes/"+id+"/revisions", nil)
		r = chiCtx(r.WithContext(withRequestID(fx.ctx, "rid-revisions")), map[string]string{"schema": "notes", "id": id})
		rr := httptest.NewRecorder()
		h.ListRevisions(rr, r)

		require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
		assert.JSONEq(t, `{"error":"failed to list revisions","code":"service_unavailable","request_id":"rid-revisions"}`,
			rr.Body.String())
		assertOneStoreFailureLine(t, buf, "rid-revisions")
	})
}
