package runtime

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// versionReads counts the reads of a session's token version.
type versionReads struct {
	db.DB
	n atomic.Int64
}

func (v *versionReads) QueryRow(ctx context.Context, q string, args ...any) (*sql.Row, error) {
	if strings.Contains(q, "token_version FROM sys_users") {
		v.n.Add(1)
	}
	return v.DB.QueryRow(ctx, q, args...)
}

// A request the engine sheds under load or turns away while it drains costs
// no database read, so the session check runs only for a request the engine
// admitted. Without shedding, the same request does read the version.
func TestRouters_ShedRequestReadsNoSessionVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds both routers on a database")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	a := startSlotPlugins(t, nil)
	pool := testdb.Postgres(t)
	cfg := chainConfig()
	cfg.BackpressureEnabled = true
	ctx := context.Background()
	lifetime, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	latency := apimw.NewLatencyTracker(200, 500)
	token, err := auth.Sign(cfg.JWTSecret, 3600, uuid.New(), "shed@test.com", []string{"admin"}, "", 1)
	require.NoError(t, err)

	shedding := apimw.BackpressureResult{Middleware: func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
	}}
	admitting := apimw.BackpressureResult{Middleware: func(next http.Handler) http.Handler { return next }}

	for _, tc := range []struct {
		name      string
		bp        apimw.BackpressureResult
		wantReads bool
	}{
		{"shed", shedding, false},
		{"admitted", admitting, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counted := &versionReads{DB: pool}
			adminOpts := buildAdminExtraMW(ctx, cfg, a, NewInflightDrainer(), tc.bp, latency)
			admin, err := api.NewAdminRouter(counted, cfg, append(adminOpts, api.WithLifetime(lifetime))...)
			require.NoError(t, err)
			apiOpts := buildAPIExtraMW(ctx, cfg, a, NewInflightDrainer(), tc.bp, latency)
			apiRouter, err := api.NewAPIRouter(counted, cfg, nil, append(apiOpts, api.WithLifetime(lifetime))...)
			require.NoError(t, err)

			for _, rt := range []struct {
				name string
				h    http.Handler
				path string
			}{
				{"admin", admin, "/api/admin/shed-probe"},
				{"api", apiRouter, "/api/v1/shed-probe"},
			} {
				counted.n.Store(0)
				req := httptest.NewRequest(http.MethodGet, rt.path, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				rec := httptest.NewRecorder()
				rt.h.ServeHTTP(rec, req)
				reads := counted.n.Load()
				if tc.wantReads {
					require.NotZero(t, reads, "%s: an admitted request read no session version", rt.name)
					continue
				}
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rt.name)
				require.Zero(t, reads, "%s: a shed request read the session version", rt.name)
			}
		})
	}
}
