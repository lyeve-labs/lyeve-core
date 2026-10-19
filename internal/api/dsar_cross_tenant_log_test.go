package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/logstream"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// entrySink keeps every entry the enrich handler hands its sink, which is
// what the log search route reads.
type entrySink struct {
	mu      sync.Mutex
	entries []observability.LogEntry
}

func (s *entrySink) Write(e observability.LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
}
func (s *entrySink) Close() error { return nil }
func (s *entrySink) Name() string { return "test" }

// engineLogs installs the handler chain the engine boots with, with tenant's
// admin having raised their own tenant's level to Debug, and returns what the
// ring and the sink receive.
func engineLogs(t *testing.T, tenant string) (*logstream.Buffer, *entrySink) {
	t.Helper()
	levels := observability.NewLogLevelMap(slog.LevelInfo)
	levels.SetTenantLevel(tenant, slog.LevelDebug)
	base := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	ring := logstream.NewBuffer(1024)
	enrich := logging.NewEnrichHandler(logstream.NewInterceptor(base, ring), levels)
	sink := &entrySink{}
	enrich.SetSink(sink)
	prev := slog.Default()
	slog.SetDefault(enrich.Logger())
	t.Cleanup(func() { slog.SetDefault(prev) })
	return ring, sink
}

// visibleTo returns every record a GroupAdmin of tenant reads: the ring
// records the stream route scopes to their tenant, and the stored entries the
// search and export routes scope the same way.
func visibleTo(t *testing.T, tenant string, ring *logstream.Buffer, sink *entrySink) []string {
	t.Helper()
	var out []string
	for _, e := range ring.Snapshot(func(e logstream.Entry) bool { return e.TenantID == tenant }, 0) {
		b, err := json.Marshal(e)
		require.NoError(t, err)
		out = append(out, string(b))
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.entries {
		if e.Attrs["tenant_id"] != tenant {
			continue
		}
		b, err := json.Marshal(e)
		require.NoError(t, err)
		out = append(out, string(b))
	}
	return out
}

// countNaming counts the ring records that mention s under any tenant.
func countNaming(ring *logstream.Buffer, s string) int {
	n := 0
	for _, e := range ring.Snapshot(nil, 0) {
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), s) {
			n++
		}
	}
	return n
}

// A super admin scoped to acme exports a subject across every tenant. The
// subject has data in globex, and one more tenant cannot be scoped. Every log
// line about those tenants still reaches the whole log, and nothing acme's
// admins read, in the ring or in stored entries, names globex, the tenant
// that failed, their data or the failure. The audit entry carries none of it
// either.
func TestDSARExport_AllTenantsLogsNothingAboutOtherTenantsToTheRequester(t *testing.T) {
	const (
		home    = "acme"
		other   = "globex"
		broken  = "initech"
		marker  = "globex-order-7731"
		failure = "initech_db is unreachable"
	)
	compliance.ResetSubjectExporters()
	t.Cleanup(compliance.ResetSubjectExporters)
	compliance.RegisterSubjectExporter(&tenantExporter{tenant: other, data: map[string]any{
		"orders": []map[string]any{{"note": marker}},
	}})
	ring, sink := engineLogs(t, home)

	lister := func(context.Context) ([]string, error) { return []string{home, other, broken}, nil }
	scope := func(ctx context.Context, tenant string) (context.Context, func(), error) {
		if tenant == broken {
			return ctx, func() {}, errors.New(failure)
		}
		return core.WithTenantID(ctx, tenant), func() {}, nil
	}
	audit := &mockAuditWriter{}
	h := NewDSARHandler(audit, lister, scope)

	r := exportReq(t, map[string]any{"identifier": "sam@example.com", "all_tenants": true})
	ctx := core.WithTenantID(r.Context(), home)
	ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: "u-1", Roles: []string{"super_admin"}, TenantID: home})
	rec := httptest.NewRecorder()
	h.Export(rec, r.WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), marker, "the export itself reached globex")

	assert.Positive(t, countNaming(ring, other), "the sweep's line about globex is still logged")
	assert.Positive(t, countNaming(ring, failure), "the scoping failure is still logged")
	for _, line := range visibleTo(t, home, ring, sink) {
		for _, leaked := range []string{other, broken, marker, failure} {
			assert.NotContains(t, line, leaked, "a record acme's admins read names another tenant")
		}
	}
	require.Len(t, audit.calls, 1)
	b, err := json.Marshal(audit.calls[0])
	require.NoError(t, err)
	for _, leaked := range []string{other, broken, marker, failure} {
		assert.NotContains(t, string(b), leaked)
	}
}

// tenantExporter returns data only in its own tenant.
type tenantExporter struct {
	tenant string
	data   map[string]any
}

func (e *tenantExporter) ExportSubject(ctx context.Context, _ string) (map[string]any, error) {
	if core.TenantIDFromCtx(ctx) != e.tenant {
		return nil, nil
	}
	return e.data, nil
}
