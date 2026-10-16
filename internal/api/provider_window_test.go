package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The window decides who can sign in, so only a signed-in session reads or
// changes it. An admin token holding every grant and an API key are refused
// with 403. An API key holding an admin role never reaches the route: the
// admin API refuses it with 401. No refused call changes the stored window.
func TestProviderWindow_RefusesAPIKeysAndAdminTokens(t *testing.T) {
	asKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			roles := []string{"editor"}
			if key == "admin-key" {
				roles = []string{"admin", "super_admin"}
			}
			claims := &core.AuthClaims{
				UserID:   uuid.NewString(),
				Roles:    roles,
				Scopes:   []string{"*"},
				TenantID: "default",
				IsAPIKey: true,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}
	rg := newTokenRig(t, WithAPIKeyAuth(asKey))
	_, session := rg.user("admin")

	var grants []string
	for _, g := range core.AdminGrants() {
		grants = append(grants, g.Name)
	}
	token, _ := rg.issue(session, grants, nil)

	const path = "/api/admin/auth/provider-window"
	require.Equal(t, http.StatusOK, rg.get(path, session), "the route is mounted and a session reaches it")

	for _, c := range []struct {
		name   string
		bearer string
		header map[string]string
		want   int
	}{
		{"api key", "", map[string]string{"X-API-Key": "editor-key"}, http.StatusForbidden},
		{"api key with an admin role", "", map[string]string{"X-API-Key": "admin-key"}, http.StatusUnauthorized},
		{"admin token", token, nil, http.StatusForbidden},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			rec := rg.do(tokenCall{method: method, path: path, bearer: c.bearer, header: c.header, body: map[string]any{"days": 0}})
			assert.Equal(t, c.want, rec.Code, "%s %s: %s", c.name, method, rec.Body.String())
		}
	}

	days, err := config.NewProviderWindows(rg.pool).ProviderWindowDays(context.Background(), "default")
	require.NoError(t, err)
	assert.Equal(t, core.ProviderWindowDefaultDays, days, "no refused call changed the window")
}

// Two saves of the window overlap. The first holds the settings row when the
// second arrives, then sets 20 over the stored 10, and the second sets 30 once
// the first commits. Each audit entry names the value its own save replaced,
// so the entries chain: the second save replaced 20, not the 10 both saves
// found when they started.
func TestProviderWindow_OverlappingSavesAuditTheValueEachReplaced(t *testing.T) {
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)

	for name, open := range settingsPools(t) {
		t.Run(name, func(t *testing.T) {
			pool := open(t)
			store := db.NewPluginConfigStore(pool)
			h := &providerWindowHandler{store: store, windows: config.NewProviderWindows(pool), host: testhost.New(pool)}
			ctx := core.WithTenantID(context.Background(), "default")
			actor := uuid.NewString()
			ctx = context.WithValue(ctx, auth.ClaimsKey, &auth.Claims{UserID: actor, Roles: []string{"super_admin"}, TenantID: "default"})
			ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: actor, Roles: []string{"super_admin"}, TenantID: "default"})
			require.NoError(t, store.Set(ctx, config.CoreSettingsOwner, map[string]any{core.ProviderWindowSettingKey: 10}, nil))
			audit.mu.Lock()
			audit.entries = nil
			audit.mu.Unlock()

			save := func(ctx context.Context, days int) error {
				req := httptest.NewRequest(http.MethodPut, "/api/admin/auth/provider-window",
					strings.NewReader(fmt.Sprintf(`{"days":%d}`, days))).WithContext(ctx)
				rec := httptest.NewRecorder()
				h.put(rec, req)
				if rec.Code != http.StatusOK {
					return fmt.Errorf("saving %d answered %d: %s", days, rec.Code, rec.Body.String())
				}
				return nil
			}

			locked, release, firstDone := make(chan error, 1), make(chan struct{}), make(chan error, 1)
			go func() {
				firstDone <- db.WriteInTransaction(ctx, pool, func(ctx context.Context) error {
					// The lock the first save's merge takes, taken before the save
					// runs, so the second save arrives while the row is held and
					// nothing is written yet.
					row, err := pool.QueryRow(ctx, db.PluginConfigLockSQL(pool.Engine()), "default", config.CoreSettingsOwner)
					if err == nil {
						var raw []byte
						err = row.Scan(&raw)
					}
					locked <- err
					if err != nil {
						return err
					}
					<-release
					return save(ctx, 20)
				})
			}()
			require.NoError(t, <-locked)

			secondDone := make(chan error, 1)
			go func() { secondDone <- save(ctx, 30) }()
			waited := true
			var secondErr error
			select {
			case secondErr = <-secondDone:
				waited = false
			case <-time.After(300 * time.Millisecond):
			}
			close(release)
			require.NoError(t, <-firstDone)
			if waited {
				secondErr = <-secondDone
			}
			require.NoError(t, secondErr)
			require.True(t, waited, "the second save did not wait for the row the first held")

			replaced := map[int]int{}
			audit.mu.Lock()
			for _, e := range audit.entries {
				if e.Action != "auth.provider_window.update" {
					continue
				}
				var before, after struct{ Days int }
				require.NoError(t, json.Unmarshal([]byte(e.BeforeJSON), &before))
				require.NoError(t, json.Unmarshal([]byte(e.AfterJSON), &after))
				replaced[after.Days] = before.Days
			}
			audit.mu.Unlock()
			assert.Equal(t, map[int]int{20: 10, 30: 20}, replaced, "each save's audit entry, keyed by the value it set, names the value it replaced")

			days, err := h.windows.ProviderWindowDays(ctx, "default")
			require.NoError(t, err)
			assert.Equal(t, 30, days)
		})
	}
}
