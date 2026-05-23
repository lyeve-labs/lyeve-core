package auth_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// JWT

func TestSign_ParseRoundTrip(t *testing.T) {
	secret := "test-secret-32-chars-minimum-len"
	id := uuid.New()
	email := "alice@example.com"
	roles := []string{"admin"}

	token, err := auth.Sign(secret, 3600, id, email, roles, "", 1)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	claims, err := auth.Parse(secret, token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.UserID != id.String() {
		t.Errorf("UserID: got %q, want %q", claims.UserID, id.String())
	}
	if claims.Email != email {
		t.Errorf("Email: got %q, want %q", claims.Email, email)
	}
	if len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Errorf("Roles: got %v", claims.Roles)
	}
	if claims.MFAPending {
		t.Error("MFAPending should be false for a full session token")
	}
}

func TestParse_WrongSecret(t *testing.T) {
	if auth.IsEdDSAActive() {
		t.Skip("EdDSA active: wrong-secret test relies on HMAC-only path")
	}
	id := uuid.New()
	token, _ := auth.Sign("correct-secret", 3600, id, "a@b.com", nil, "", 1)

	if _, err := auth.Parse("wrong-secret", token); err == nil {
		t.Error("expected error for wrong secret, got nil")
	}
}

func TestSign_UniquePerIssuance(t *testing.T) {
	// A session token must be unique per issuance: the jti claim guarantees a
	// refresh issues a different sys_session cookie than login even when both
	// happen within the same second (where iat/exp are second-granular and
	// would otherwise collide), so a refresh always rotates the session
	// cookie.
	secret := "test-secret-32-chars-minimum-len"
	id := uuid.New()
	tok1, err := auth.Sign(secret, 3600, id, "a@b.com", nil, "", 1)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	tok2, err := auth.Sign(secret, 3600, id, "a@b.com", nil, "", 1)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if tok1 == tok2 {
		t.Error("two signings of the same claims must produce distinct tokens (jti)")
	}

	claims, err := auth.Parse(secret, tok1)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.ID == "" {
		t.Error("session token must carry a jti (ID) claim")
	}
}

func TestParse_ExpiredToken(t *testing.T) {
	id := uuid.New()
	// expiry of -1 second = already expired
	token, err := auth.Sign("secret", -1, id, "a@b.com", nil, "", 1)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := auth.Parse("secret", token); err == nil {
		t.Error("expected error for expired token, got nil")
	}
}

func TestParse_MalformedToken(t *testing.T) {
	if _, err := auth.Parse("secret", "not.a.jwt"); err == nil {
		t.Error("expected error for malformed token, got nil")
	}
}

func TestParseMulti_FirstSecretWins(t *testing.T) {
	id := uuid.New()
	token, _ := auth.Sign("old-secret", 3600, id, "a@b.com", nil, "", 1)

	claims, err := auth.ParseMulti([]string{"new-secret", "old-secret"}, token)
	if err != nil {
		t.Fatalf("ParseMulti: %v", err)
	}
	if claims.UserID != id.String() {
		t.Errorf("UserID mismatch: %s", claims.UserID)
	}
}

func TestParseMulti_NoSecretsMatch(t *testing.T) {
	if auth.IsEdDSAActive() {
		t.Skip("EdDSA active: no-secrets-match test relies on HMAC-only path")
	}
	id := uuid.New()
	token, _ := auth.Sign("secret", 3600, id, "a@b.com", nil, "", 1)
	if _, err := auth.ParseMulti([]string{"wrong1", "wrong2"}, token); err == nil {
		t.Error("expected error when no secrets match")
	}
}

func TestParseMulti_EmptyList(t *testing.T) {
	if _, err := auth.ParseMulti(nil, "any"); err == nil {
		t.Error("expected error for empty secrets list")
	}
}

func TestSignChallenge_MFAPending(t *testing.T) {
	id := uuid.New()
	token, err := auth.SignChallenge("secret", id, "a@b.com", []string{"user"}, "")
	if err != nil {
		t.Fatalf("SignChallenge: %v", err)
	}
	claims, err := auth.ParseMulti([]string{"secret"}, token)
	if err != nil {
		t.Fatalf("ParseMulti: %v", err)
	}
	if !claims.MFAPending {
		t.Error("MFAPending should be true for a challenge token")
	}
}

func TestSignChallenge_ShortExpiry(t *testing.T) {
	id := uuid.New()
	token, _ := auth.SignChallenge("secret", id, "a@b.com", nil, "")

	// Parse raw claims to verify expiry is ≤ 5 minutes from now.
	raw, _, err := new(jwt.Parser).ParseUnverified(token, &auth.Claims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	c := raw.Claims.(*auth.Claims)
	exp := c.ExpiresAt.Time
	if exp.After(time.Now().Add(6 * time.Minute)) {
		t.Errorf("challenge token expires too far in the future: %v", exp)
	}
}

func TestClaims_HasRole(t *testing.T) {
	c := &auth.Claims{Roles: []string{"admin", "editor"}}
	if !c.HasRole("admin") {
		t.Error("HasRole(admin) should be true")
	}
	if c.HasRole("super_admin") {
		t.Error("HasRole(super_admin) should be false")
	}
}

// Password

func TestHashVerify_Bcrypt(t *testing.T) {
	hash, err := auth.HashPassword("bcrypt", "mysecretpassword")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := auth.VerifyPassword("bcrypt", hash, "mysecretpassword"); err != nil {
		t.Errorf("VerifyPassword correct: %v", err)
	}
	if err := auth.VerifyPassword("bcrypt", hash, "wrongpassword"); err == nil {
		t.Error("VerifyPassword wrong password should fail")
	}
}

func TestHashVerify_Argon2id(t *testing.T) {
	hash, err := auth.HashPassword("argon2id", "mysecretpassword")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := auth.VerifyPassword("argon2id", hash, "mysecretpassword"); err != nil {
		t.Errorf("VerifyPassword correct: %v", err)
	}
	if err := auth.VerifyPassword("argon2id", hash, "wrongpassword"); err == nil {
		t.Error("VerifyPassword wrong password should fail")
	}
}

func TestHashVerify_CrossAlgo(t *testing.T) {
	// bcrypt hash is auto-detected regardless of algo param in VerifyPassword.
	hash, _ := auth.HashPassword("bcrypt", "pass")
	if err := auth.VerifyPassword("argon2id", hash, "pass"); err != nil {
		t.Errorf("cross-algo verify should work via prefix detection: %v", err)
	}
}

func TestHashVerify_UnknownAlgoFallsToBcrypt(t *testing.T) {
	hash, err := auth.HashPassword("sha512", "pass")
	if err != nil {
		t.Fatalf("unknown algo should fall back to bcrypt: %v", err)
	}
	if err := auth.VerifyPassword("", hash, "pass"); err != nil {
		t.Errorf("bcrypt fallback verify failed: %v", err)
	}
}
