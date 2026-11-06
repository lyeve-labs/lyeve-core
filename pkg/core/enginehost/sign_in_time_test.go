package enginehost

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// The passkey, magic-link, OAuth and SAML sign-ins all end here, so the
// session the host signs carries the time it was signed as its sign-in time.
func TestEngineHost_SignSessionTokenStampsAuthTime(t *testing.T) {
	host := &engineHost{cfg: &config.Config{JWTSecret: jwtTestSecret, JWTExpirySecs: 3600}}

	before := time.Now().Unix()
	token, err := host.SignSessionToken(context.Background(), uuid.New(), "passkey@example.com", []string{"editor"})
	require.NoError(t, err)
	after := time.Now().Unix()

	claims, err := auth.Parse(jwtTestSecret, token)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, claims.AuthTime, before)
	assert.LessOrEqual(t, claims.AuthTime, after)
}
