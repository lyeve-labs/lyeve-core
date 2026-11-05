package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// An access line names who made the request by id and the route that served
// it, so it joins the audit log and the account without the audit log's help.
// A route that takes an identifier in its path is logged by its pattern alone,
// because the identifier can be an address. With tracing off the line carries
// no trace ids, which would be all zeros.
func TestAccessLog_NamesTheActorAndTheRoute(t *testing.T) {
	const suppressions = "/api/admin/email/suppressions/{email}"
	userID, keyID := uuid.NewString(), uuid.NewString()
	session := &auth.Claims{UserID: userID, Email: "ann@example.com", Roles: []string{"admin"}, TenantID: "acme"}
	key := &core.AuthClaims{UserID: uuid.NewString(), Email: "apikey:ci", IsAPIKey: true, APIKeyID: keyID, TenantID: "acme"}

	for _, tc := range []struct {
		name      string
		session   *auth.Claims
		key       *core.AuthClaims
		target    string
		wantRoute any
		wantPath  any
		wantActor any
	}{
		{"session on a route with an identifier", session, nil, "/api/admin/email/suppressions/bob@example.com", suppressions, nil, userID},
		{"api key on a route with an identifier", nil, key, "/api/admin/email/suppressions/bob@example.com", suppressions, nil, keyID},
		{"session on a fixed route", session, nil, "/api/admin/schemas", "/api/admin/schemas", "/api/admin/schemas", userID},
		{"anonymous unrouted path", nil, nil, "/no/such/route", nil, "/no/such/route", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := captureAccessLog(t)
			ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
			r := chi.NewRouter()
			r.Use(structuredLogger)
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := r.Context()
					if tc.session != nil {
						ctx = context.WithValue(ctx, auth.ClaimsKey, tc.session)
						ctx = context.WithValue(ctx, core.ClaimsKey, tc.session.AuthClaims())
					}
					if tc.key != nil {
						ctx = context.WithValue(ctx, core.ClaimsKey, tc.key)
					}
					next.ServeHTTP(w, r.WithContext(ctx))
				})
			})
			r.Use(apimw.TenantHeader(true))
			r.Get(suppressions, ok)
			r.Get("/api/admin/schemas", ok)

			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.target, nil))

			lines := cap.requestLines()
			require.Len(t, lines, 1)
			line := lines[0]
			assert.Equal(t, tc.wantRoute, line["route"])
			assert.Equal(t, tc.wantPath, line["path"])
			assert.Equal(t, tc.wantActor, line["actor"])
			assert.NotContains(t, line, "trace_id")
			assert.NotContains(t, line, "span_id")
			for k, v := range line {
				if s, isString := v.(string); isString {
					assert.NotContains(t, s, "@example.com", "field %q carries an address", k)
				}
			}
		})
	}
}
