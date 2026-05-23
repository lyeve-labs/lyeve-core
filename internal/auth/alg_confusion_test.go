package auth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// withEdDSA activates EdDSA for one test and puts the package back as it was.
//
// The signing key is package state and there is no reset, so a test that
// leaves EdDSA on changes what every later test in this package accepts.
func withEdDSA(t *testing.T) {
	t.Helper()

	signingKeyMu.Lock()
	oldKey, oldKid, oldActive := signingKey, signingKid, eddsaActive
	signingKeyMu.Unlock()

	t.Cleanup(func() {
		signingKeyMu.Lock()
		signingKey, signingKid, eddsaActive = oldKey, oldKid, oldActive
		signingKeyMu.Unlock()
	})

	if err := InitJWTSigning(filepath.Join(t.TempDir(), "jwt_key.json")); err != nil {
		t.Fatalf("init signing: %v", err)
	}
	if !IsEdDSAActive() {
		t.Fatal("EdDSA is not active, so this proves nothing")
	}
}

// The configured algorithm, never the token's header, decides what verifies.
// With EdDSA active, an HS256 token signed with JWT_SECRET is refused.
func TestParse_RefusesHS256WhenEdDSAIsActive(t *testing.T) {
	withEdDSA(t)

	const secret = "change-me-to-a-32-plus-character-jwt-secret"

	// The issuer and audience have to be right, or the parser refuses the
	// token for that and the algorithm never comes into it.
	now := time.Now()
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{
		UserID:   uuid.New().String(),
		TenantID: "any-tenant",
		Roles:    []string{"super_admin"},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{security.JWTAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	})
	signed, err := forged.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := Parse(secret, signed); err == nil {
		t.Fatal("an HS256 token was accepted on an EdDSA install, so JWT_SECRET is a way in")
	}
}

// The refusal is narrow: it takes away the caller's choice of algorithm and
// nothing else, so the token the engine issues still verifies.
func TestParse_AcceptsTheTokenTheEngineIssues(t *testing.T) {
	withEdDSA(t)

	const secret = "a-secret-long-enough-for-the-check"
	signed, err := Sign(secret, 3600, uuid.New(), "a@b.test", []string{"admin"}, "t", 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := Parse(secret, signed); err != nil {
		t.Fatalf("the engine's own token must verify: %v", err)
	}
}

// An install that opted out of EdDSA keeps working, because there the HMAC
// token is the configured one rather than the caller's choice.
func TestParse_AcceptsHS256WhereItIsTheConfiguredAlgorithm(t *testing.T) {
	signingKeyMu.Lock()
	oldKey, oldKid, oldActive := signingKey, signingKid, eddsaActive
	signingKey, signingKid, eddsaActive = nil, "", false
	signingKeyMu.Unlock()
	t.Cleanup(func() {
		signingKeyMu.Lock()
		signingKey, signingKid, eddsaActive = oldKey, oldKid, oldActive
		signingKeyMu.Unlock()
	})

	const secret = "a-secret-long-enough-for-the-check"
	signed, err := Sign(secret, 3600, uuid.New(), "a@b.test", []string{"admin"}, "t", 0)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := Parse(secret, signed); err != nil {
		t.Fatalf("an HS256 install must keep working: %v", err)
	}
}
