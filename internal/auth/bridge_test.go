package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// configOnlyHost is a host that publishes its secrets through Config and
// implements no SecretsProvider, which is what every plugin test host is.
type configOnlyHost struct {
	core.Host
	cfg core.Config
}

func (h configOnlyHost) Config() core.Config { return h.cfg }

type listConfig struct {
	core.Config
	strings map[string][]string
	str     map[string]string
}

func (c listConfig) Strings(key string) []string { return c.strings[key] }
func (c listConfig) String(key string) string    { return c.str[key] }
func (c listConfig) Bool(string) bool            { return false }

func signed(t *testing.T, secret string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-1",
		"iss": security.JWTIssuer,
		"aud": []string{security.JWTAudience},
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	s, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// TestVerifyJWT_ReadsTheRotationListOffConfig pins that the fallback reads the
// rotation list, "jwt_secrets", from Config. A host that publishes the list
// only through Config, as a plugin's test host does, must still verify a good
// token.
func TestVerifyJWT_ReadsTheRotationListOffConfig(t *testing.T) {
	const secret = "active-key"
	host := configOnlyHost{cfg: listConfig{strings: map[string][]string{"jwt_secrets": {secret}}}}

	claims, err := security.VerifyJWT(host, signed(t, secret))
	if err != nil {
		t.Fatalf("a token signed with a configured secret was refused: %v", err)
	}
	if claims.UserID != "u-1" {
		t.Errorf("UserID = %q, want u-1", claims.UserID)
	}
}

// TestVerifyJWT_AcceptsAnyKeyInTheList is the whole point of a rotation list:
// a token signed with the outgoing key keeps working while the new one is
// being handed out.
func TestVerifyJWT_AcceptsAnyKeyInTheList(t *testing.T) {
	host := configOnlyHost{cfg: listConfig{
		strings: map[string][]string{"jwt_secrets": {"new-key", "old-key"}},
	}}

	for _, secret := range []string{"new-key", "old-key"} {
		if _, err := security.VerifyJWT(host, signed(t, secret)); err != nil {
			t.Errorf("a token signed with %q was refused: %v", secret, err)
		}
	}
}

// A host that publishes one secret under the singular jwt_secret key verifies
// with it.
func TestVerifyJWT_ReadsTheSingularKey(t *testing.T) {
	const secret = "only-key"
	host := configOnlyHost{cfg: listConfig{str: map[string]string{"jwt_secret": secret}}}

	if _, err := security.VerifyJWT(host, signed(t, secret)); err != nil {
		t.Fatalf("a token signed with the singular secret was refused: %v", err)
	}
}

// TestVerifyJWT_RefusesWhenNothingIsConfigured is the control. Widening where
// the secrets are read from must not widen what is accepted: a host with no
// secret at all still verifies nothing.
func TestVerifyJWT_RefusesWhenNothingIsConfigured(t *testing.T) {
	host := configOnlyHost{cfg: listConfig{}}

	if _, err := security.VerifyJWT(host, signed(t, "some-key")); err == nil {
		t.Fatal("a host with no configured secret accepted a token")
	}
}

// TestVerifyJWT_RefusesAKeyThatIsNotInTheList is the second control. Reading
// the list must not mean trusting a signature made with something else.
func TestVerifyJWT_RefusesAKeyThatIsNotInTheList(t *testing.T) {
	host := configOnlyHost{cfg: listConfig{
		strings: map[string][]string{"jwt_secrets": {"configured-key"}},
	}}

	if _, err := security.VerifyJWT(host, signed(t, "attacker-key")); err == nil {
		t.Fatal("a token signed with an unconfigured key was accepted")
	}
}
