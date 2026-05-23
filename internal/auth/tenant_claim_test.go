package auth_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// Tokens have to carry the signer's tenant, because TenantHeader reads
// claims.TenantID and has no other way to learn which tenant a password
// login belongs to. Without it a tenant-scoped user authenticates
// successfully and then runs with no tenant on the context at all: the
// search_path stays on public and every sys_* query filters on the empty
// string.

func TestSign_CarriesTenantThroughRoundTrip(t *testing.T) {
	const secret = "tenant-claim-round-trip-secret-32b"
	id := uuid.New()

	token, err := auth.Sign(secret, 3600, id, "member@acme.test", []string{"admin"}, "acme", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.Equal(t, "acme", claims.TenantID,
		"a token signed for a tenant user must name that tenant")
	assert.Equal(t, "acme", claims.AuthClaims().TenantID,
		"the plugin-facing view of the claims must agree")
}

// A super_admin has no home tenant. The claim is omitempty, so its token
// carries no tenant_id at all, and the X-Tenant-ID override remains the only
// way for it to pick one.
func TestSign_OmitsTheClaimWhenThereIsNoTenant(t *testing.T) {
	const secret = "tenant-claim-omitted-secret-32byte"
	id := uuid.New()

	token, err := auth.Sign(secret, 3600, id, "root@example.com", []string{"super_admin"}, "", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.Empty(t, claims.TenantID)
}

// The MFA challenge is the only credential the verify handler has if the user
// lookup fails, so it has to carry the tenant too.
func TestSignChallenge_CarriesTenant(t *testing.T) {
	const secret = "tenant-claim-challenge-secret-32by"
	id := uuid.New()

	token, err := auth.SignChallenge(secret, id, "member@acme.test", []string{"editor"}, "acme")
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.Equal(t, "acme", claims.TenantID)
	assert.True(t, claims.MFAPending)
}
