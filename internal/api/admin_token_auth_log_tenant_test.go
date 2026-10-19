package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// failingOwners fails every owner read the way a database that cannot answer
// does.
type failingOwners struct{ owner *domain.User }

func (f failingOwners) GetByID(context.Context, uuid.UUID) (*domain.User, error) {
	if f.owner != nil {
		return f.owner, nil
	}
	return nil, errors.New("owner read failed")
}

// failingMemberships fails every membership read.
type failingMemberships struct{}

func (failingMemberships) Get(context.Context, uuid.UUID, string) (*core.Membership, error) {
	return nil, errors.New("membership read failed")
}

func (failingMemberships) ListForUser(context.Context, uuid.UUID) ([]*core.Membership, error) {
	return nil, errors.New("membership read failed")
}

// An admin token of globex arrives on a domain mapped to acme, and acme's
// admin has raised acme's level to Debug. Token auth runs before tenancy, so
// the request's context holds acme. When the owner or the owner's membership
// cannot be read, the line names globex's token, so it goes under no tenant.
// The same failure for a token of acme keeps acme's stamp.
func TestAdminTokenAuth_LinesAboutAnotherTenantsTokenCarryNoTenant(t *testing.T) {
	const home, other = "acme", "globex"
	ring, sink := engineLogs(t, home)

	member := &domain.User{ID: uuid.New(), TenantID: home, Roles: []string{"admin"}}
	cases := map[string]*adminTokenAuth{
		"owner read fails":      {users: failingOwners{}, now: time.Now},
		"membership read fails": {users: failingOwners{owner: member}, members: failingMemberships{}, now: time.Now},
	}
	for name, a := range cases {
		tok := &core.AdminToken{ID: uuid.New(), TenantID: other, OwnerUserID: member.ID, ExpiresAt: time.Now().Add(time.Hour)}
		r := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
		r = r.WithContext(core.WithTenantID(r.Context(), home))
		_, status, _ := a.admit(r, tok, "192.0.2.1")
		require.Equal(t, http.StatusServiceUnavailable, status, name)
		assert.Equal(t, 1, countNaming(ring, tok.ID.String()), "%s: the line is still logged", name)
		for _, line := range visibleTo(t, home, ring, sink) {
			assert.NotContains(t, line, tok.ID.String(), name)
		}
	}

	own := &core.AdminToken{ID: uuid.New(), TenantID: home, OwnerUserID: member.ID, ExpiresAt: time.Now().Add(time.Hour)}
	r := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
	r = r.WithContext(core.WithTenantID(r.Context(), home))
	_, status, _ := (&adminTokenAuth{users: failingOwners{}, now: time.Now}).admit(r, own, "192.0.2.1")
	require.Equal(t, http.StatusServiceUnavailable, status)
	mine := 0
	for _, line := range visibleTo(t, home, ring, sink) {
		if assert.Contains(t, line, own.ID.String()) {
			mine++
		}
	}
	assert.Equal(t, 2, mine, "a line about acme's own token keeps acme's stamp")
}
