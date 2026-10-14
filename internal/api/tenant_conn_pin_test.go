package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// pinPoolSize is the pool every test here runs on. One idle stream per
// tenant in four tenants is enough to hold all of it if a stream keeps the
// connection its request was scoped to.
const pinPoolSize = 4

// pinRig is a multi-tenant API router served over HTTP on a small pool,
// with a plugin route that streams until its client leaves and one that
// reads once.
type pinRig struct {
	srv    *httptest.Server
	pool   db.DB
	users  *db.UserStore
	hooks  *hooks.Registry
	secret string
}

func newPinRig(t *testing.T, pool db.DB) *pinRig {
	t.Helper()
	cfg := testConfig()
	cfg.DatabaseDriver = pool.Engine()
	cfg.MultiTenant = true

	stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": open\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row, err := pool.QueryRow(r.Context(), `SELECT COUNT(*) FROM sys_users WHERE tenant_id = $1`, core.TenantIDFromCtx(r.Context()))
		var n int
		if err == nil {
			err = row.Scan(&n)
		}
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	reg := hooks.NewRegistry()
	router, err := NewAPIRouter(pool, cfg, reg, WithLifetime(testLifetime(t)),
		WithPluginRoutes([]plugin.PluginRoutes{{
			Name: "pin-probe",
			Routes: []plugin.RouteDecl{
				{Method: http.MethodGet, Pattern: "/api/v1/pin-probe/stream", Group: plugin.GroupAuth, Handler: stream},
				{Method: http.MethodGet, Pattern: "/api/v1/pin-probe/read", Group: plugin.GroupAuth, Handler: read},
			},
		}}))
	require.NoError(t, err)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &pinRig{srv: srv, pool: pool, users: db.NewUserStore(pool), hooks: reg, secret: cfg.JWTSecret}
}

// session creates a super admin in tenant and returns the user and a session
// token for them.
func (p *pinRig) session(t *testing.T, tenant string) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	u, err := p.users.Create(ctx, uuid.NewString()+"@pin.test", "!hash!", []string{"super_admin"}, tenant)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.users.Delete(ctx, u.ID) })
	tok, err := auth.Sign(p.secret, 3600, u.ID, u.Email, u.Roles, tenant, u.TokenVersion)
	require.NoError(t, err)
	return u.ID, tok
}

// get sends a GET with tok and returns the response, which the caller closes.
func (p *pinRig) get(ctx context.Context, t *testing.T, path, tok string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.srv.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := p.srv.Client().Do(req)
	require.NoError(t, err)
	return res
}

// open starts a stream and returns its lines as they arrive, after the
// first one has.
func (p *pinRig) open(t *testing.T, path, tok string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	res := p.get(ctx, t, path, tok)
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("the stream was refused at the open: %d %s", res.StatusCode, body)
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if sc.Text() != "" {
				lines <- sc.Text()
			}
		}
	}()
	select {
	case <-lines:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never sent its first line")
	}
	return lines
}

// requirePoolFree waits for every pool connection the streams might hold to
// come back.
func (p *pinRig) requirePoolFree(t *testing.T, what string) {
	t.Helper()
	assert.Eventually(t, func() bool { return p.pool.Stats().InUse == 0 }, 3*time.Second, 20*time.Millisecond,
		"%s hold %d of %d pool connections", what, p.pool.Stats().InUse, pinPoolSize)
}

// requireOtherTenantServed reads once with tok, a session in a tenant with no
// stream open, inside a deadline a request starved of a connection runs into.
func (p *pinRig) requireOtherTenantServed(t *testing.T, tok string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.srv.URL+"/api/v1/pin-probe/read", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := p.srv.Client().Do(req)
	require.NoError(t, err, "a read in a tenant with no stream open did not finish inside its deadline")
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
}

// bumpWithin ends the user's sessions inside a deadline, which a pool the
// streams hold would otherwise make wait forever.
func bumpWithin(p *pinRig, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return p.users.BumpTokenVersion(ctx, id)
}

// pinDialects runs fn on each dialect the run covers, on a pool of
// pinPoolSize connections.
func pinDialects(t *testing.T, fn func(t *testing.T, pool db.DB)) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	for _, d := range []struct {
		name string
		pool func(*testing.T, int) db.DB
	}{
		{"postgres", testdb.PostgresWithMaxConns},
		{"mysql", testdb.MySQLWithMaxConns},
		{"mssql", testdb.MSSQLWithMaxConns},
	} {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			fn(t, d.pool(t, pinPoolSize))
		})
	}
}

// An authenticated route that streams, as a plugin's event stream or socket
// does, kept the connection the session check took for its request until the
// client went away. Four idle streams in four tenants then held the whole
// pool, and a fifth tenant's read waited until its deadline. A stream that
// reads nothing holds nothing, and the session check still ends a session
// that was revoked.
func TestAPIRouter_SessionStreamHoldsNoPoolConnection(t *testing.T) {
	pinDialects(t, func(t *testing.T, pool db.DB) {
		rig := newPinRig(t, pool)
		_, other := rig.session(t, "pin_e")
		var firstUser uuid.UUID
		var firstToken string
		for i, tenant := range []string{"pin_a", "pin_b", "pin_c", "pin_d"} {
			id, tok := rig.session(t, tenant)
			if i == 0 {
				firstUser, firstToken = id, tok
			}
			rig.open(t, "/api/v1/pin-probe/stream", tok)
		}
		rig.requirePoolFree(t, "four idle session streams")
		rig.requireOtherTenantServed(t, other)

		require.NoError(t, bumpWithin(rig, firstUser))
		res := rig.get(context.Background(), t, "/api/v1/pin-probe/stream", firstToken)
		defer res.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, res.StatusCode, "a stream opened with a revoked session")
	})
}

// The content change stream asks again before each change whether the
// caller's credential and grants still admit it. Those reads ran on the
// connection the request was scoped to, so a stream that had delivered one
// change kept that connection until its client went away. A stream holds no
// connection between changes, and a session revoked while it is open still
// ends it at the next change.
func TestContentStream_HoldsNoPoolConnectionBetweenChanges(t *testing.T) {
	pinDialects(t, func(t *testing.T, pool db.DB) {
		rig := newPinRig(t, pool)
		_, other := rig.session(t, "pin_e")
		tenants := []string{"pin_a", "pin_b", "pin_c", "pin_d"}
		streams := make([]<-chan string, len(tenants))
		var firstUser uuid.UUID
		for i, tenant := range tenants {
			id, tok := rig.session(t, tenant)
			if i == 0 {
				firstUser = id
			}
			streams[i] = rig.open(t, "/api/v1/content/notes/stream", tok)
		}
		rig.requirePoolFree(t, "four change streams before any change")
		change := func(tenant, title string) {
			require.NoError(t, rig.hooks.Run(core.WithTenantID(context.Background(), tenant),
				hooks.Event{Type: hooks.AfterUpdate, Schema: "notes", Data: map[string]any{"title": title}}))
		}
		next := func(i int) string {
			for {
				select {
				case line, ok := <-streams[i]:
					if !ok {
						t.Fatalf("stream %d ended", i)
					}
					if !strings.HasPrefix(line, ":") {
						return line
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("stream %d sent nothing for the change", i)
				}
			}
		}
		for i, tenant := range tenants {
			change(tenant, "first")
			assert.Contains(t, next(i), "first")
		}
		rig.requirePoolFree(t, "four change streams between changes")
		rig.requireOtherTenantServed(t, other)

		require.NoError(t, bumpWithin(rig, firstUser))
		change(tenants[0], "after the sign-out")
		assert.Equal(t, "event: error", next(0), "a stream whose session was revoked")
	})
}
