package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// cappedStreamHandler is a stream handler with the router's slot counters.
func cappedStreamHandler() *ContentHandler {
	return &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: true}}, streamSlots: new(streamSlots), streamDeadlineWarn: new(sync.Once)}
}

// asSession is a session of a new account in tenantID, so streams opened on
// it share one credential and no other test's streams count against it.
func asSession(tenantID string) context.Context {
	claims := &auth.Claims{UserID: uuid.NewString(), Roles: []string{"editor"}, TokenType: "session", TokenVersion: 3}
	return context.WithValue(tenant.WithID(context.Background(), tenantID), auth.ClaimsKey, claims)
}

// refusedStream opens a stream that the handler must refuse and returns the
// response. A stream it admits by mistake is cut off after a second, so the
// test fails rather than hangs.
func refusedStream(t *testing.T, h *ContentHandler, base context.Context) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(base, time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/content/notes/stream", nil)
	r = chiCtx(r.WithContext(ctx), map[string]string{"schema": "notes"})
	rr := httptest.NewRecorder()
	h.Stream(rr, r)
	return rr
}

func requireStreamCapped(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusTooManyRequests, rr.Code, rr.Body.String())
	assert.Equal(t, "10", rr.Header().Get("Retry-After"))
	assert.Contains(t, rr.Body.String(), "too many open change streams")
	assert.NotContains(t, rr.Body.String(), "connected")
}

// openStreams opens n streams as the caller on base and returns a function
// that ends the i-th and waits for its handler to return.
func openStreams(t *testing.T, h *ContentHandler, base context.Context, n int) func(i int) {
	t.Helper()
	ends := make([]context.CancelFunc, n)
	dones := make([]<-chan struct{}, n)
	for i := range n {
		ctx, cancel := context.WithCancel(base)
		_, dones[i] = openStreamDone(t, h, ctx, "notes")
		ends[i] = cancel
	}
	return func(i int) {
		ends[i]()
		<-dones[i]
	}
}

// One token could hold any number of streams open, and every write to the
// schema then cost one credential read per stream on the pool every tenant
// shares. A credential holds at most eight, the ninth is refused with a 429
// that says when to try again, and a stream the client leaves gives its slot
// back.
func TestContentHandler_Stream_CapsStreamsPerCredential(t *testing.T) {
	t.Parallel()
	require.Equal(t, 8, contentStreamsPerCredential)
	h := cappedStreamHandler()
	tenantID := uuid.NewString()
	session := asSession(tenantID)

	end := openStreams(t, h, session, 8)
	requireStreamCapped(t, refusedStream(t, h, session))

	// Another account in the same tenant has a cap of its own.
	openStreamAs(t, h, asSession(tenantID), "notes")

	end(0)
	openStreamAs(t, h, session, "notes")
	requireStreamCapped(t, refusedStream(t, h, session))
}

// An API key is one credential however many streams it opens, and every
// credential of a tenant together holds at most 128.
func TestContentHandler_Stream_CapsStreamsPerTenant(t *testing.T) {
	t.Parallel()
	require.Equal(t, 128, contentStreamsPerTenant)
	h := cappedStreamHandler()
	tenantID := uuid.NewString()

	key := &core.AuthClaims{UserID: "key-" + tenantID, APIKeyID: "key-" + tenantID, Roles: []string{"editor"}, Scopes: []string{"content:read"}, IsAPIKey: true}
	asTenantKey := context.WithValue(tenant.WithID(context.Background(), tenantID), core.ClaimsKey, key)
	openStreams(t, h, asTenantKey, 8)
	requireStreamCapped(t, refusedStream(t, h, asTenantKey))

	for range 128 - 8 {
		openStreamAs(t, h, asSession(tenantID), "notes")
	}
	requireStreamCapped(t, refusedStream(t, h, asSession(tenantID)))

	// Another tenant's cap is its own.
	openStreamAs(t, h, asSession(uuid.NewString()), "notes")
}

// A stream that ends on its own gives its slot back: one whose token reaches
// its expiry, and every stream of a session that is ended while they are open.
func TestContentHandler_Stream_FreesItsSlotWhenTheCredentialEnds(t *testing.T) {
	t.Parallel()
	t.Run("expiry", func(t *testing.T) {
		t.Parallel()
		h := cappedStreamHandler()
		session := asSession("t1")
		claims, ok := session.Value(auth.ClaimsKey).(*auth.Claims)
		require.True(t, ok)

		openStreams(t, h, session, 7)
		expiring := *claims
		expiring.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Second))
		expired, done := openStreamDone(t, h, context.WithValue(session, auth.ClaimsKey, &expiring), "notes")
		requireStreamCapped(t, refusedStream(t, h, session))

		requireEndedByCredential(t, expired, done)
		openStreamAs(t, h, session, "notes")
		requireStreamCapped(t, refusedStream(t, h, session))
	})
	t.Run("revocation", func(t *testing.T) {
		t.Parallel()
		h := cappedStreamHandler()
		var version atomic.Int64
		version.Store(3)
		h.streamCredential = streamCredential(func(context.Context, string) (int, error) { return int(version.Load()), nil }, nil)
		session := asSession("t1")

		streams := make([]*tenantStreamWriter, 8)
		dones := make([]<-chan struct{}, 8)
		for i := range 8 {
			streams[i], dones[i] = openStreamDone(t, h, session, "notes")
		}
		requireStreamCapped(t, refusedStream(t, h, session))

		version.Store(4)
		streamChange(t, h, "notes", "after the sign-out")
		for i := range 8 {
			requireEndedByCredential(t, streams[i], dones[i])
			assert.NotContains(t, streams[i].String(), "after the sign-out")
		}

		// The handler does not authenticate the open, so the same token opens
		// again here, and finds every slot it held free.
		version.Store(3)
		openStreams(t, h, session, 8)
		requireStreamCapped(t, refusedStream(t, h, session))
	})
}

// A stream that panics still gives its slot back, so a fault in one event
// cannot leave a credential locked out until the instance restarts.
func TestContentHandler_Stream_FreesItsSlotOnPanic(t *testing.T) {
	t.Parallel()
	h := cappedStreamHandler()
	h.streamCredential = func(r *http.Request) error {
		if r.Header.Get("Panic") != "" {
			panic("credential store fault")
		}
		return nil
	}
	session := asSession("t1")
	openStreams(t, h, session, 7)

	stream := newTenantStreamWriter()
	r := chiCtx(httptest.NewRequest(http.MethodGet, "/api/v1/content/notes/stream", nil).WithContext(session), map[string]string{"schema": "notes"})
	r.Header.Set("Panic", "1")
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		h.Stream(stream, r)
	}()
	require.Eventually(t, func() bool { return stream.String() != "" }, 5*time.Second, 10*time.Millisecond, "stream never connected")
	requireStreamCapped(t, refusedStream(t, h, session))

	streamChange(t, h, "notes", "boom")
	require.Equal(t, "credential store fault", <-panicked)
	openStreamAs(t, h, session, "notes")
}

// countingSchemas answers every schema as one without draft and publish and
// counts the lookups, each a read the engine makes for every event.
type countingSchemas struct {
	core.SchemaSource
	reads atomic.Int64
}

func (s *countingSchemas) GetByName(context.Context, string) (*core.Schema, error) {
	s.reads.Add(1)
	return &core.Schema{}, nil
}

// Fifty streams on one token turned one write into fifty credential reads
// and fifty schema lookups. With the cap, the token holds eight streams, so
// one write costs eight of each.
func TestContentHandler_Stream_OneWriteCostsAtMostTheCap(t *testing.T) {
	t.Parallel()
	h := cappedStreamHandler()
	schemas := &countingSchemas{}
	var credentialReads atomic.Int64
	h.schemas = schemas
	h.streamCredential = streamCredential(func(context.Context, string) (int, error) {
		credentialReads.Add(1)
		return 3, nil
	}, nil)
	session := asSession("t1")

	var open []*tenantStreamWriter
	for range 50 {
		if rr := refusedStreamOrOpen(t, h, session); rr != nil {
			open = append(open, rr)
		}
	}
	credentialReads.Store(0)
	schemas.reads.Store(0)
	streamChange(t, h, "notes", "one write")
	for _, s := range open {
		require.Eventually(t, func() bool { return len(s.events(t)) == 1 }, 5*time.Second, 10*time.Millisecond)
	}
	t.Logf("50 streams requested, %d open: one write cost %d credential reads and %d schema lookups",
		len(open), credentialReads.Load(), schemas.reads.Load())
	assert.Len(t, open, 8)
	assert.Equal(t, int64(8), credentialReads.Load())
	assert.Equal(t, int64(8), schemas.reads.Load())
}

// refusedStreamOrOpen opens a stream and returns it, or nil when the handler
// refuses it with a 429.
func refusedStreamOrOpen(t *testing.T, h *ContentHandler, base context.Context) *tenantStreamWriter {
	t.Helper()
	ctx, cancel := context.WithCancel(base)
	r := chiCtx(httptest.NewRequest(http.MethodGet, "/api/v1/content/notes/stream", nil).WithContext(ctx), map[string]string{"schema": "notes"})
	w := newTenantStreamWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Stream(w, r)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool { return w.String() != "" }, 5*time.Second, 10*time.Millisecond, "stream never answered")
	if strings.Contains(w.String(), "too many open change streams") {
		return nil
	}
	return w
}

// The router builds the content handler with its slot counters, so the caps
// hold on the route a client reaches and not only on a handler a test builds.
func TestAPIRouter_ContentStreamCapsAKey(t *testing.T) {
	t.Parallel()
	keyID := uuid.NewString()
	key := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := &core.AuthClaims{UserID: keyID, APIKeyID: keyID, Roles: []string{"editor"}, Scopes: []string{"content:read"}, IsAPIKey: true}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}
	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)), WithAPIKeyAuth(key),
		WithPermissionChecker(fakeProvider{fakeChecker{allow: true}}))
	require.NoError(t, err)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	get := func() *http.Response {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/content/notes/stream", nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", "stream-cap-key")
		res, err := srv.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}
	for i := range 8 {
		require.Equal(t, http.StatusOK, get().StatusCode, "stream %d", i+1)
	}
	res := get()
	assert.Equal(t, http.StatusTooManyRequests, res.StatusCode)
	assert.Equal(t, "10", res.Header.Get("Retry-After"))
}
