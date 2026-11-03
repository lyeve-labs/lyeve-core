//go:build !mutest

package api

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin reads an account's roles in a tenant the way a sign-in there
// resolves them: home without a grant, anywhere else only with one, and a
// grant's roles over the row's.
func TestTenantRoles_ResolvesAsASignInDoes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})
	ctx := context.Background()

	roles, err := f.handler.TenantRoles(ctx, f.userID, "brand_a")
	require.NoError(t, err)
	assert.Equal(t, []string{"editor"}, roles, "home needs no grant")

	for _, tenant := range []string{"brand_b", "BRAND_A", "Brand_b", ""} {
		_, err := f.handler.TenantRoles(ctx, f.userID, tenant)
		assert.ErrorIs(t, err, core.ErrNotFound, "tenant %q", tenant)
	}

	_, err = f.members.Grant(ctx, f.userID, "brand_b", []string{"viewer"})
	require.NoError(t, err)
	roles, err = f.handler.TenantRoles(ctx, f.userID, "brand_b")
	require.NoError(t, err)
	assert.Equal(t, []string{"viewer"}, roles, "the grant's roles, not the row's")

	_, err = f.members.Grant(ctx, f.userID, "brand_a", []string{"admin"})
	require.NoError(t, err)
	roles, err = f.handler.TenantRoles(ctx, f.userID, "brand_a")
	require.NoError(t, err)
	assert.Equal(t, []string{"admin"}, roles, "a home grant overrides the row")

	require.NoError(t, f.members.Revoke(ctx, f.userID, "brand_b"))
	_, err = f.handler.TenantRoles(ctx, f.userID, "brand_b")
	assert.ErrorIs(t, err, core.ErrNotFound, "a revoked grant is none")

	_, err = f.handler.TenantRoles(ctx, uuid.New(), "brand_a")
	assert.ErrorIs(t, err, core.ErrNotFound, "an account that does not exist")
}

// A super admin acts in any tenant on the row alone, as at sign-in. An
// account whose row names no home tenant belongs to the default tenant.
func TestTenantRoles_SuperAdminAndTheDefaultHome(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	ctx := context.Background()
	f := newMembershipLoginFixture(t, "brand_a", []string{"super_admin"})
	roles, err := f.handler.TenantRoles(ctx, f.userID, "brand_z")
	require.NoError(t, err)
	assert.Equal(t, []string{"super_admin"}, roles)

	g := newMembershipLoginFixtureOn(t, f.pool, "", []string{"editor"})
	roles, err = g.handler.TenantRoles(ctx, g.userID, core.DefaultTenantSlug)
	require.NoError(t, err)
	assert.Equal(t, []string{"editor"}, roles)
}

// A read that failed is an outage the caller may retry, never a refusal it
// would act on.
func TestTenantRoles_AFailedReadIsAnOutage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	ctx := context.Background()
	f := newMembershipLoginFixture(t, "brand_a", []string{"editor"})

	f.handler.WithMemberships(failingMemberships{})
	for _, tenant := range []string{"brand_a", "brand_b"} {
		_, err := f.handler.TenantRoles(ctx, f.userID, tenant)
		assert.ErrorIs(t, err, core.ErrServiceUnavailable, tenant)
		assert.NotErrorIs(t, err, core.ErrNotFound, tenant)
	}

	f.handler.WithMemberships(f.members)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := f.handler.TenantRoles(canceled, f.userID, "brand_a")
	assert.ErrorIs(t, err, core.ErrServiceUnavailable, "the account read failed")
	assert.NotErrorIs(t, err, core.ErrNotFound)
}

// tenantRolesHost records the reader the admin router hands it.
type tenantRolesHost struct {
	core.Host
	mu     sync.Mutex
	reader core.TenantRolesReader
}

func (h *tenantRolesHost) SetTenantRolesReader(r core.TenantRolesReader) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reader = r
}

// The admin router hands the engine host the handler that resolves roles at
// sign-in, so a plugin's answer and a sign-in's never differ.
func TestTenantRoles_TheAdminRouterWiresTheSignInResolver(t *testing.T) {
	host := &tenantRolesHost{}
	rg := newTokenRig(t, WithScalingHost(host))
	host.mu.Lock()
	reader := host.reader
	host.mu.Unlock()
	require.NotNil(t, reader, "the admin router wired no roles reader")

	u, _ := rg.userIn("brand_a", "editor")
	roles, err := reader.TenantRoles(context.Background(), u.ID, "brand_a")
	require.NoError(t, err)
	assert.Equal(t, []string{"editor"}, roles)
	_, err = reader.TenantRoles(context.Background(), u.ID, "brand_b")
	assert.ErrorIs(t, err, core.ErrNotFound)
}
