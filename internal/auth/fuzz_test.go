// fuzz_test.go: Go native fuzz harnesses for auth/crypto parsers.
//
// Verifies that Parse, DecryptSecret, parseJWK, and HashKey never panic
// on arbitrary, truncated, or adversarial inputs.
//
// Run:
//
//	go test -fuzz=FuzzJWTParse -fuzztime=30s ./internal/auth/
//	go test -fuzz=FuzzDecryptWireFormat -fuzztime=30s ./internal/auth/
//	go test -fuzz=FuzzJWKSParse -fuzztime=30s ./internal/auth/
//	go test -fuzz=FuzzAPIKeyDecode -fuzztime=30s ./internal/auth/
//	make fuzz-short          # 30s each
//	make fuzz-auth           # 60s each

package auth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// Helpers

const fuzzTestSecret = "fuzz-test-secret-32-chars-minimum-length-ok"

func setupTestEdDSAKey(t testing.TB) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	kid := deriveKid(pub)
	return priv, kid
}

func makeHS256Token(t testing.TB) string {
	t.Helper()
	id := uuid.New()
	now := time.Now()
	claims := Claims{
		UserID: id.String(),
		Email:  "fuzz@example.com",
		Roles:  []string{"admin"},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{security.JWTAudience},
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(fuzzTestSecret))
	if err != nil {
		t.Fatalf("sign HS256 token: %v", err)
	}
	return s
}

// Uses a throwaway key, not the package-level signing key.
func makeEdDSAToken(t testing.TB) string {
	t.Helper()
	priv, kid := setupTestEdDSAKey(t)
	id := uuid.New()
	now := time.Now()
	claims := Claims{
		UserID: id.String(),
		Email:  "fuzz-eddsa@example.com",
		Roles:  []string{"viewer"},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{security.JWTAudience},
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign EdDSA token: %v", err)
	}
	return s
}

func repeatStr(s string, n int) string {
	var b strings.Builder
	b.Grow(len(s) * n)
	for i := 0; i < n; i++ {
		b.WriteString(s)
	}
	return b.String()
}

// FuzzJWTParse

// Covers HS256 and EdDSA paths. Asserts no panic, no OOM.
func FuzzJWTParse(f *testing.F) {
	// Seed corpus: valid tokens and adversarial edge cases.
	f.Add(makeHS256Token(f))                        // valid HS256
	f.Add(makeEdDSAToken(f))                        // valid EdDSA (unverifiable without package key, but no panic)
	f.Add("")                                       // empty
	f.Add("not.a.jwt")                              // three parts but garbage
	f.Add("a")                                      // single segment
	f.Add("a.b")                                    // two segments
	f.Add("a.b.c.d")                                // four segments
	f.Add("eyJhbG...wIn0.")                         // alg=none
	f.Add("eyJhbG...NiJ9." + repeatStr("A", 10000)) // oversized payload
	f.Add(string([]byte{0xff, 0xfe, 0xfd}))         // invalid UTF-8

	f.Fuzz(func(t *testing.T, tokenStr string) {
		Parse(fuzzTestSecret, tokenStr)
		ParseMulti([]string{fuzzTestSecret, "rotation-secret"}, tokenStr)
	})
}

// FuzzDecryptWireFormat

// Wire format (hex-encoded):
//
//	New:      salt (16 bytes) || nonce (12 bytes) || ciphertext || GCM tag (16 bytes)
//	Legacy:   nonce (12 bytes) || ciphertext || GCM tag (16 bytes)
//
// Asserts no panic, no OOM.
func FuzzDecryptWireFormat(f *testing.F) {
	pass := "fuzz-encryption-passphrase"

	valid, err := EncryptSecret("hello world", pass)
	if err == nil {
		f.Add(valid, pass)         // valid new-format (PBKDF2)
		f.Add(valid, "wrong-pass") // wrong passphrase
		f.Add(valid, "")           // empty passphrase
	}

	// Adversarial seed entries.
	f.Add("", pass)                        // empty ciphertext
	f.Add("zzzz", pass)                    // invalid hex
	f.Add("00112233", pass)                // valid hex, too short
	f.Add(repeatStr("00", 100), pass)      // all-zeros, valid hex
	f.Add(repeatStr("deadbeef", 50), pass) // garbage hex, larger
	f.Add("aabbccdd", "")                  // empty passphrase, short input

	f.Fuzz(func(t *testing.T, encHex, passphrase string) {
		DecryptSecret(encHex, passphrase)
	})
}

// FuzzJWKSParse

// parseJWK handles RSA and EC key deserialization from untrusted JWKS endpoints.
// Asserts no panic, no OOM.
func FuzzJWKSParse(f *testing.F) {
	// Valid RSA JWK seed.
	f.Add("RSA", "AQAB", "rsa-modulus-base64url-value-here", "", "", "")
	// Valid EC P-256 JWK seed.
	f.Add("EC", "", "", "P-256", "ec-x-coord", "ec-y-coord")
	// Unsupported key type.
	f.Add("oct", "", "", "", "", "")
	// Empty everything.
	f.Add("", "", "", "", "", "")
	// EC with unsupported curve.
	f.Add("EC", "", "", "Curve25519", "xval", "yval")
	// RSA with invalid base64 in N.
	f.Add("RSA", "AQAB", "!!!invalid-base64!!!", "", "", "")
	// RSA with empty N.
	f.Add("RSA", "AQAB", "", "", "", "")

	f.Fuzz(func(t *testing.T, kty, e, n, crv, x, y string) {
		k := jwk{
			Kty: kty,
			E:   e,
			N:   n,
			Crv: crv,
			X:   x,
			Y:   y,
		}
		pub, err := parseJWK(k)
		if err != nil {
			return
		}
		switch pub.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey:
			// ok
		default:
			t.Errorf("parseJWK returned unexpected type: %T", pub)
		}
	})
}

// FuzzAPIKeyDecode

// HashKey processes untrusted X-API-Key header values.
// Asserts consistent output length and deterministic results.
func FuzzAPIKeyDecode(f *testing.F) {
	raw, _, _ := security.GenerateKey()
	f.Add(raw)                                           // valid ly_* key
	f.Add("")                                            // empty
	f.Add("short")                                       // too short
	f.Add("not-a-real-key")                              // wrong prefix
	f.Add("cms_" + hex.EncodeToString(make([]byte, 64))) // valid format, zero key

	f.Fuzz(func(t *testing.T, rawKey string) {
		hash := security.HashKey(rawKey)
		if len(hash) != 64 {
			t.Errorf("HashKey length = %d, want 64 (hex SHA-256)", len(hash))
		}
		hash2 := security.HashKey(rawKey)
		if hash != hash2 {
			t.Errorf("HashKey not deterministic: %q != %q", hash, hash2)
		}
	})
}
