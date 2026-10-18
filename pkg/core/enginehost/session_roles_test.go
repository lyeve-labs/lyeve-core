// A plugin sign-in signs its session through SignSessionToken, so the roles
// in that session have to be the ones the engine's own sign-in gives the
// account at home. A membership for the home tenant overrides the user row
// there, which is how a super admin demotes an account in its own tenant.
package enginehost

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// homeMembers answers one membership per tenant, or err for every lookup.
type homeMembers struct {
	t        *testing.T
	byTenant map[string][]string
	err      error
}

func (m homeMembers) Get(_ context.Context, userID uuid.UUID, tenantID string) (*core.Membership, error) {
	if m.err != nil {
		return nil, m.err
	}
	roles, ok := m.byTenant[tenantID]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &core.Membership{UserID: userID, TenantID: tenantID, Roles: roles}, nil
}

func (m homeMembers) ListForUser(context.Context, uuid.UUID) ([]*core.Membership, error) {
	m.t.Fatal("the signer lists no memberships")
	return nil, nil
}

func sessionRolesFollowHome(t *testing.T, pool db.DB) {
	ctx := context.Background()

	// roles is TEXT[] on PostgreSQL and JSON on the other two.
	adminRoles := `["admin"]`
	if pool.Engine() == "postgres" {
		adminRoles = "{admin}"
	}
	userID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, tenant_id, token_version)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, "session-roles-"+userID.String()[:8]+"@example.com", "x", adminRoles, "acme", 1)
	require.NoError(t, err)

	host := &engineHost{
		cfg:   &config.Config{JWTSecret: jwtTestSecret, JWTExpirySecs: 3600},
		pool:  pool,
		rawDB: pool.SQLDB(),
	}
	// The caller asks for more than the account holds. It is never signed.
	asked := []string{"super_admin"}
	signedRoles := func(t *testing.T) []string {
		t.Helper()
		token, err := host.SignSessionToken(ctx, userID, "session-roles@example.com", asked)
		require.NoError(t, err)
		claims, err := security.ParseJWT(jwtTestSecret, token)
		require.NoError(t, err)
		assert.Equal(t, "acme", claims.TenantID)
		return claims.Roles
	}

	t.Run("no reader signs the row's roles", func(t *testing.T) {
		host.SetAdminSeatMembers(nil)
		assert.Equal(t, []string{"admin"}, signedRoles(t))
	})

	t.Run("a home membership overrides the row", func(t *testing.T) {
		host.SetAdminSeatMembers(homeMembers{t: t, byTenant: map[string][]string{"acme": {"viewer"}}})
		assert.Equal(t, []string{"viewer"}, signedRoles(t))
	})

	t.Run("a home membership never adds super_admin", func(t *testing.T) {
		host.SetAdminSeatMembers(homeMembers{t: t, byTenant: map[string][]string{"acme": {"super_admin", "viewer"}}})
		assert.Equal(t, []string{"viewer"}, signedRoles(t))
	})

	t.Run("a membership elsewhere leaves the row's roles", func(t *testing.T) {
		host.SetAdminSeatMembers(homeMembers{t: t, byTenant: map[string][]string{"globex": {"viewer"}}})
		assert.Equal(t, []string{"admin"}, signedRoles(t))
	})

	t.Run("a home membership never drops super_admin", func(t *testing.T) {
		superRoles := `["super_admin"]`
		if pool.Engine() == "postgres" {
			superRoles = "{super_admin}"
		}
		superID := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO sys_users (id, email, password_hash, roles, tenant_id, token_version)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			superID, "session-roles-"+superID.String()[:8]+"@example.com", "x", superRoles, "acme", 1)
		require.NoError(t, err)

		host.SetAdminSeatMembers(homeMembers{t: t, byTenant: map[string][]string{"acme": {"admin"}}})
		token, err := host.SignSessionToken(ctx, superID, "session-roles@example.com", nil)
		require.NoError(t, err)
		claims, err := security.ParseJWT(jwtTestSecret, token)
		require.NoError(t, err)
		assert.Equal(t, []string{"admin", "super_admin"}, claims.Roles)
	})

	t.Run("a reader failure refuses to sign", func(t *testing.T) {
		down := errors.New("membership store down")
		host.SetAdminSeatMembers(homeMembers{t: t, err: down})
		token, err := host.SignSessionToken(ctx, userID, "session-roles@example.com", asked)
		require.ErrorIs(t, err, down)
		assert.Empty(t, token)
	})
}

func TestSessionRolesFollowHome_Postgres(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("skipping postgres (CI_DIALECT restriction)")
	}
	sessionRolesFollowHome(t, testdb.Postgres(t))
}

func TestSessionRolesFollowHome_MySQL(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("skipping mysql (CI_DIALECT restriction)")
	}
	sessionRolesFollowHome(t, testdb.MySQL(t))
}

func TestSessionRolesFollowHome_MSSQL(t *testing.T) {
	if !testdb.ShouldTest("mssql") {
		t.Skip("skipping mssql (CI_DIALECT restriction)")
	}
	sessionRolesFollowHome(t, testdb.MSSQL(t))
}
