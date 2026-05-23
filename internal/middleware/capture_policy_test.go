package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// recordingPolicy answers a fixed decision and records what it was asked.
type recordingPolicy struct {
	mu      sync.Mutex
	ttl     time.Duration
	capture bool
	asked   []string
}

func (p *recordingPolicy) CaptureFor(tenantID, method, path string) (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, tenantID+" "+method+" "+path)
	return p.ttl, p.capture
}

var _ core.CapturePolicy = (*recordingPolicy)(nil)

// servePolicy runs one request through the capture middleware with policy,
// resolving the tenant below it the way TenantHeader does.
func servePolicy(t *testing.T, policy core.CapturePolicy, tenant, method, target string) *testSink {
	t.Helper()
	sink := &testSink{}
	h := middleware.RequestCaptureWithPolicy(sink, nil, policy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slot := core.TenantSlotFrom(r.Context()); slot != nil {
			slot.Set(tenant)
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, target, nil)
	ctx, _ := core.WithTenantSlot(req.Context())
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	return sink
}

func TestRequestCaptureWithPolicy_NilPolicyKeepsEveryRequestForTheDefault(t *testing.T) {
	sink := servePolicy(t, nil, "acme", http.MethodGet, "/api/v1/posts?page=2")

	entry := sink.Last()
	require.NotNil(t, entry)
	assert.Equal(t, core.DefaultCaptureTTL, entry.TTL)
	assert.Equal(t, 24*time.Hour, entry.TTL, "the default is the day every install has kept")
	assert.Equal(t, "acme", entry.TenantID)
}

func TestRequestCaptureWithPolicy_AsksWithTheResolvedTenantAndBarePath(t *testing.T) {
	policy := &recordingPolicy{capture: true}
	servePolicy(t, policy, "acme", http.MethodPost, "/api/v1/orders?expand=lines")

	assert.Equal(t, []string{"acme POST /api/v1/orders"}, policy.asked)
}

func TestRequestCaptureWithPolicy_DeclinedRequestNeverReachesTheSink(t *testing.T) {
	policy := &recordingPolicy{capture: false, ttl: time.Hour}
	sink := servePolicy(t, policy, "acme", http.MethodGet, "/api/v1/health")

	assert.Equal(t, 0, sink.Count())
}

func TestRequestCaptureWithPolicy_RetentionComesFromThePolicy(t *testing.T) {
	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"thirty days", 30 * 24 * time.Hour, 30 * 24 * time.Hour},
		{"one hour", time.Hour, time.Hour},
		{"zero keeps the default", 0, core.DefaultCaptureTTL},
		{"negative keeps the default", -time.Minute, core.DefaultCaptureTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := servePolicy(t, &recordingPolicy{capture: true, ttl: tc.ttl}, "acme", http.MethodGet, "/api/v1/posts")
			entry := sink.Last()
			require.NotNil(t, entry)
			assert.Equal(t, tc.want, entry.TTL)
		})
	}
}

// A policy decides what is kept, never what is withheld: a credential body
// stays out of a capture the policy asked to keep for a month.
func TestRequestCaptureWithPolicy_CannotReleaseWithheldBodies(t *testing.T) {
	sink := &testSink{}
	policy := &recordingPolicy{capture: true, ttl: 30 * 24 * time.Hour}
	h := middleware.RequestCaptureWithPolicy(sink, nil, policy)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"shown-once"}`))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", nil))

	entry := sink.Last()
	require.NotNil(t, entry)
	assert.Empty(t, entry.ResponseBody)
}

// The policy is one interface call that allocates nothing, so a request
// costs the same allocations with no policy and with one that keeps
// everything.
func TestRequestCaptureWithPolicy_NilPolicyAddsNoAllocation(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	sink := &countingSink{}
	measure := func(policy core.CapturePolicy) float64 {
		h := middleware.RequestCaptureWithPolicy(sink, nil, policy)(next)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/posts", nil)
		w := httptest.NewRecorder()
		return testing.AllocsPerRun(200, func() { h.ServeHTTP(w, req) })
	}
	withoutPolicy := measure(nil)
	withPolicy := measure(keepAll{})
	assert.Equal(t, withPolicy, withoutPolicy)
}

type keepAll struct{}

func (keepAll) CaptureFor(string, string, string) (time.Duration, bool) {
	return core.DefaultCaptureTTL, true
}

// countingSink stores nothing, so the measurement above counts the
// middleware's own allocations and not a growing slice.
type countingSink struct{ n int }

func (s *countingSink) Capture(observability.CaptureEntry) error {
	s.n++
	return nil
}
