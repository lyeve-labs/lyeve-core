package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Another issuer's token is refused when it could never expire, and when its
// tenant claim is no tenant slug, which on MySQL and MSSQL would name a real
// tenant under a second spelling.
func TestParseExternal_RefusesUnboundedOrMisnamedClaims(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k", pub: pub}})

	sign := func(tenant string, exp *jwt.NumericDate) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, Claims{
			UserID:   "external-user",
			Email:    "idp@test.com",
			Roles:    []string{"editor"},
			TenantID: tenant,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "external-user",
				Issuer:    srv.URL,
				Audience:  jwt.ClaimStrings{security.JWTAudience},
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				ExpiresAt: exp,
			},
		})
		tok.Header["kid"] = "k"
		s, err := tok.SignedString(priv)
		require.NoError(t, err)
		return s
	}
	inAnHour := jwt.NewNumericDate(time.Now().Add(time.Hour))

	for _, tc := range []struct {
		name   string
		token  string
		refuse bool
	}{
		{"no expiry", sign("acme", nil), true},
		{"a spelling no tenant has", sign("ACME", inAnHour), true},
		{"a hyphenated name", sign("acme-corp", inAnHour), true},
		{"a slug", sign("acme", inAnHour), false},
		{"no tenant", sign("", inAnHour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseExternal(context.Background(), tc.token, jwksURLFor(t, srv), srv.URL)
			if tc.refuse {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
