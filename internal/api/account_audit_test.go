//go:build !mutest

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// An auditor asks who created an account, changed its roles, disabled it,
// set its password and deleted it. Each change is filed under the account's
// tenant with the super admin who made it and the address they made it from,
// and no entry carries the password.
func TestUsersHandler_AccountChangesAreAudited(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	if testing.Short() {
		t.Skip("integration test")
	}
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)

	pool := testdb.Postgres(t)
	h := NewUsersHandler(db.NewUserStore(pool))
	h.host = auditOnlyHost{}

	const tenant = "audit-tenant"
	admin := uuid.New()
	call := func(method, path, body string, params map[string]string, handle http.HandlerFunc) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "198.51.100.7:4100"
		ctx := core.WithTenantID(r.Context(), tenant)
		ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: admin.String(), Roles: []string{"super_admin"}, TenantID: tenant})
		r = chiCtx(r.WithContext(ctx), params)
		rr := httptest.NewRecorder()
		handle(rr, r)
		return rr
	}

	const password = "An-audited-password-1"
	rr := call(http.MethodPost, "/api/admin/users", `{"email":"audited@example.com","password":"`+password+`","roles":["editor"]}`, nil, h.Create)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var created map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &created))
	id := created["id"].(string)
	params := map[string]string{"id": id}

	rr = call(http.MethodPut, "/api/admin/users/"+id+"/roles", `{"roles":["admin"]}`, params, h.UpdateRoles)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = call(http.MethodPut, "/api/admin/users/"+id+"/state", `{"disabled":true}`, params, h.UpdateState)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = call(http.MethodPut, "/api/admin/users/"+id+"/password", `{"password":"`+password+`-2"}`, params, h.SetPassword)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = call(http.MethodDelete, "/api/admin/users/"+id, "", params, h.Delete)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	got := audit.actions()
	for _, action := range []string{"user.create", "user.roles.update", "user.state.update", "user.password.set", "user.delete"} {
		e, ok := got[action]
		if !assert.True(t, ok, "no %s entry", action) {
			continue
		}
		assert.Equal(t, "user", e.ResourceType, action)
		assert.Equal(t, id, e.ResourceID, action)
		assert.Equal(t, tenant, e.TenantID, action)
		assert.Equal(t, "198.51.100.7", e.IP, action)
		require.IsType(t, &uuid.UUID{}, e.UserID, action)
		assert.Equal(t, admin, *e.UserID.(*uuid.UUID), action)
		for _, state := range []string{e.BeforeJSON, e.AfterJSON} {
			assert.NotContains(t, state, password, action)
			assert.NotContains(t, state, "$2", "%s carries a hash", action)
		}
	}
	assert.JSONEq(t, `{"roles":["editor"]}`, got["user.roles.update"].BeforeJSON)
	assert.JSONEq(t, `{"roles":["admin"]}`, got["user.roles.update"].AfterJSON)
	assert.Contains(t, got["user.state.update"].AfterJSON, `"disabled":true`)
	assert.JSONEq(t, `{"roles":["admin"]}`, got["user.delete"].BeforeJSON)
}
