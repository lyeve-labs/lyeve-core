package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func unsignedToken(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

// Validate refuses super_admin, but Admit holds the line on its own in case a
// policy reaches it by another road.
func TestIssuerPolicy_AdmitNeverGrantsSuperAdmin(t *testing.T) {
	p := IssuerPolicy{Issuer: "https://idp", Tenant: "agency", RoleMap: map[string]string{"root": "super_admin", "eds": "editor"}}
	roles, tenant, err := p.Admit(unsignedToken(t, map[string]any{"roles": []string{"root", "eds"}}), &Claims{UserID: "u1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"editor"}, roles)
	assert.Equal(t, "agency", tenant)
}

func TestIssuerPolicy_Admit(t *testing.T) {
	p := IssuerPolicy{Issuer: "https://idp", Tenant: "agency", RolesClaim: "groups", RoleMap: map[string]string{"eds": "editor"}}
	for name, tc := range map[string]struct {
		payload map[string]any
		claims  Claims
		ok      bool
	}{
		"mapped":             {map[string]any{"groups": []any{"eds", 7, "x"}}, Claims{UserID: "u1"}, true},
		"groups as a string": {map[string]any{"groups": "eds"}, Claims{UserID: "u1"}, true},
		"groups not a list":  {map[string]any{"groups": map[string]any{"a": 1}}, Claims{UserID: "u1"}, false},
		"other tenant":       {map[string]any{}, Claims{UserID: "u1", TenantID: "other"}, false},
		"no subject":         {map[string]any{}, Claims{}, false},
		"sub twice":          {map[string]any{"sub": "a", "SUB": "b"}, Claims{UserID: "b"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := p.Admit(unsignedToken(t, tc.payload), &tc.claims)
			assert.Equal(t, tc.ok, err == nil, "err=%v", err)
		})
	}
}

func TestEmailVerified(t *testing.T) {
	assert.True(t, EmailVerified(unsignedToken(t, map[string]any{"email_verified": true})))
	assert.False(t, EmailVerified(unsignedToken(t, map[string]any{"email_verified": "true"})))
	assert.False(t, EmailVerified(unsignedToken(t, map[string]any{})))
}

// bcrypt would reject the marker too, so the assertion is on the reason: the
// account is refused because it is an issuer's, not because the hash is odd.
func TestVerifyPassword_RefusesAnIssuerAccount(t *testing.T) {
	for _, pw := range []string{"", ExternalIssuerMarker, "anything"} {
		err := VerifyPassword("", ExternalIssuerMarker, pw)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "trusted issuer")
	}
}
