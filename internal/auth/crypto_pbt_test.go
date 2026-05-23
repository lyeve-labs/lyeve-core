package auth_test

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
	"pgregory.net/rapid"
)

// AES-256-GCM Property-Based Tests

// Property: decrypt(encrypt(x)) == x for all random plaintexts and passphrases.
func TestPBT_EncryptDecryptRoundTrip(t *testing.T) {
	if skipExpensivePBT() {
		t.Skip("PBKDF2 is too slow under -race; tested without -race")
	}
	rapid.Check(t, func(t *rapid.T) {
		// Short strings: PBKDF2 with 600K iterations is expensive per call.
		plaintext := rapid.StringN(1, 50, 50).Draw(t, "plaintext")
		passphrase := rapid.StringMatching(`[a-zA-Z0-9_]{8,24}`).Draw(t, "passphrase")

		enc, err := security.EncryptSecret(plaintext, passphrase)
		if err != nil {
			t.Fatalf("EncryptSecret: %v", err)
		}

		dec, err := security.DecryptSecret(enc, passphrase)
		if err != nil {
			t.Fatalf("DecryptSecret: %v", err)
		}

		if dec != plaintext {
			t.Fatalf("round-trip failed: got %q, want %q", dec, plaintext)
		}
	})
}

// Property: encrypting the same plaintext twice produces distinct ciphertexts.
func TestPBT_EncryptDistinctCiphertexts(t *testing.T) {
	if skipExpensivePBT() {
		t.Skip("PBKDF2 is too slow under -race; tested without -race")
	}
	rapid.Check(t, func(t *rapid.T) {
		plaintext := rapid.StringN(1, 30, 30).Draw(t, "plaintext")
		passphrase := rapid.StringMatching(`[a-zA-Z0-9]{12,20}`).Draw(t, "passphrase")

		enc1, err := security.EncryptSecret(plaintext, passphrase)
		if err != nil {
			t.Fatalf("first encrypt: %v", err)
		}
		enc2, err := security.EncryptSecret(plaintext, passphrase)
		if err != nil {
			t.Fatalf("second encrypt: %v", err)
		}

		if enc1 == enc2 {
			t.Fatalf("identical ciphertexts from same plaintext (nonce/salt collision)")
		}
	})
}

// Property: N encryptions produce distinct nonces (bytes 16..28 after salt).
func TestPBT_EncryptDistinctNonces(t *testing.T) {
	if skipExpensivePBT() {
		t.Skip("PBKDF2 is too slow under -race; tested without -race")
	}
	rapid.Check(t, func(t *rapid.T) {
		plaintext := rapid.StringN(1, 20, 20).Draw(t, "plaintext")
		passphrase := rapid.StringMatching(`[a-zA-Z0-9]{12,20}`).Draw(t, "passphrase")
		n := rapid.IntRange(2, 10).Draw(t, "n")

		seen := make(map[[12]byte]bool, n)

		for i := 0; i < n; i++ {
			enc, err := security.EncryptSecret(plaintext, passphrase)
			if err != nil {
				t.Fatalf("encrypt %d: %v", i, err)
			}
			raw, err := hex.DecodeString(enc)
			if err != nil {
				t.Fatalf("decode hex %d: %v", i, err)
			}
			if len(raw) < 16+12 {
				t.Fatalf("ciphertext too short: %d bytes", len(raw))
			}
			var nonce [12]byte
			copy(nonce[:], raw[16:28])

			if seen[nonce] {
				t.Fatalf("nonce collision at iteration %d", i)
			}
			seen[nonce] = true
		}
	})
}

// Property: decryption with the wrong passphrase always fails.
func TestPBT_DecryptWrongPassphraseFails(t *testing.T) {
	if skipExpensivePBT() {
		t.Skip("PBKDF2 is too slow under -race; tested without -race")
	}
	rapid.Check(t, func(t *rapid.T) {
		plaintext := rapid.StringN(1, 20, 20).Draw(t, "plaintext")
		passphrase := rapid.StringMatching(`[a-zA-Z0-9]{12,20}`).Draw(t, "passphrase")
		wrongPass := rapid.StringMatching(`[a-zA-Z0-9]{12,20}`).Draw(t, "wrongPass")
		if wrongPass == passphrase {
			wrongPass = passphrase + "X"
		}

		enc, err := security.EncryptSecret(plaintext, passphrase)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}

		_, err = security.DecryptSecret(enc, wrongPass)
		if err == nil {
			t.Fatalf("decryption with wrong passphrase should fail")
		}
	})
}

// Property: auth.EncryptSecret/DecryptSecret wrappers round-trip correctly.
func TestPBT_AuthEncryptDecryptRoundTrip(t *testing.T) {
	if skipExpensivePBT() {
		t.Skip("PBKDF2 is too slow under -race; tested without -race")
	}
	rapid.Check(t, func(t *rapid.T) {
		plaintext := rapid.StringN(1, 20, 20).Draw(t, "plaintext")
		passphrase := rapid.StringMatching(`[a-zA-Z0-9_]{8,24}`).Draw(t, "passphrase")

		enc, err := auth.EncryptSecret(plaintext, passphrase)
		if err != nil {
			t.Fatalf("auth.EncryptSecret: %v", err)
		}

		dec, err := auth.DecryptSecret(enc, passphrase)
		if err != nil {
			t.Fatalf("auth.DecryptSecret: %v", err)
		}

		if dec != plaintext {
			t.Fatalf("round-trip failed: got %q, want %q", dec, plaintext)
		}
	})
}

// Property: ciphertext is valid hex and has minimum expected length.
func TestPBT_EncryptOutputFormat(t *testing.T) {
	if skipExpensivePBT() {
		t.Skip("PBKDF2 is too slow under -race; tested without -race")
	}
	rapid.Check(t, func(t *rapid.T) {
		plaintext := rapid.StringN(0, 50, 50).Draw(t, "plaintext")
		passphrase := rapid.StringMatching(`[a-zA-Z0-9]{8,20}`).Draw(t, "passphrase")

		enc, err := security.EncryptSecret(plaintext, passphrase)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}

		raw, err := hex.DecodeString(enc)
		if err != nil {
			t.Fatalf("output is not valid hex: %v", err)
		}

		// Minimum: salt(16) + nonce(12) + tag(16) = 44 bytes
		minLen := 16 + 12 + 16
		if len(raw) < minLen {
			t.Fatalf("ciphertext too short: %d bytes, want >= %d", len(raw), minLen)
		}
	})
}

// JWT Property-Based Tests

// Property: sign->verify round-trips for random claims (HMAC-SHA256 path).
func TestPBT_JWTSignParseRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.StringMatching(`[a-zA-Z0-9]{32,64}`).Draw(t, "secret")
		userID := uuid.New()
		email := rapid.StringMatching(`[a-z]{3,10}@[a-z]{3,10}\.com`).Draw(t, "email")
		nRoles := rapid.IntRange(1, 5).Draw(t, "nRoles")
		roles := make([]string, nRoles)
		for i := range roles {
			roles[i] = rapid.StringMatching(`[a-z]{3,12}`).Draw(t, "role")
		}

		token, err := auth.Sign(secret, 3600, userID, email, roles, "", 1)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}

		claims, err := auth.Parse(secret, token)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if claims.UserID != userID.String() {
			t.Errorf("UserID: got %q, want %q", claims.UserID, userID.String())
		}
		if claims.Email != email {
			t.Errorf("Email: got %q, want %q", claims.Email, email)
		}
		if len(claims.Roles) != len(roles) {
			t.Errorf("Roles length: got %d, want %d", len(claims.Roles), len(roles))
		}
		if claims.Issuer != security.JWTIssuer {
			t.Errorf("Issuer: got %q, want %q", claims.Issuer, security.JWTIssuer)
		}
	})
}

// Property: sign->verify for challenge tokens (MFA) round-trips.
func TestPBT_JWTChallengeSignParseRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.StringMatching(`[a-zA-Z0-9]{32,64}`).Draw(t, "secret")
		userID := uuid.New()
		email := rapid.StringMatching(`[a-z]{3,10}@[a-z]{3,10}\.com`).Draw(t, "email")
		roles := []string{"user"}

		token, err := auth.SignChallenge(secret, userID, email, roles, "")
		if err != nil {
			t.Fatalf("SignChallenge: %v", err)
		}

		claims, err := auth.Parse(secret, token)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if !claims.MFAPending {
			t.Error("MFAPending should be true for challenge token")
		}
		if claims.UserID != userID.String() {
			t.Errorf("UserID: got %q, want %q", claims.UserID, userID.String())
		}
	})
}

// Property: a tampered token is always rejected.
func TestPBT_JWTTamperedTokenRejected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		secret := rapid.StringMatching(`[a-zA-Z0-9]{32,64}`).Draw(t, "secret")
		userID := uuid.New()
		email := "test@example.com"
		roles := []string{"admin"}

		token, err := auth.Sign(secret, 3600, userID, email, roles, "", 1)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}

		parts := strings.SplitN(token, ".", 3)
		if len(parts) != 3 {
			t.Fatalf("unexpected JWT format: %d parts", len(parts))
		}

		b := []byte(parts[1])
		b[0] = b[0] ^ 0xFF
		parts[1] = string(b)
		tampered := strings.Join(parts, ".")

		_, err = auth.Parse(secret, tampered)
		if err == nil {
			t.Fatalf("tampered token should be rejected")
		}
	})
}

// Property: token signed with one secret is rejected by a different secret.
// NOTE: HMAC-only. EdDSA signs with keypair, not the secret parameter.
func TestPBT_JWTWrongSecretRejected(t *testing.T) {
	if auth.IsEdDSAActive() {
		t.Skip("PBT_JWTWrongSecretRejected is HMAC-only; EdDSA ignores the secret parameter")
	}
	rapid.Check(t, func(t *rapid.T) {
		secret1 := rapid.StringMatching(`[a-zA-Z0-9]{32,64}`).Draw(t, "secret1")
		secret2 := rapid.StringMatching(`[a-zA-Z0-9]{32,64}`).Draw(t, "secret2")
		if secret1 == secret2 {
			secret2 = secret2 + "different"
		}
		userID := uuid.New()

		token, err := auth.Sign(secret1, 3600, userID, "test@example.com", []string{"user"}, "", 1)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}

		_, err = auth.Parse(secret2, token)
		if err == nil {
			t.Fatalf("token signed with secret1 should be rejected with secret2")
		}
	})
}

// Encryption DEK/KEK Property-Based Tests

// Property: KeyStore.DeriveTenantDEK produces distinct keys for distinct tenants.
func TestPBT_DEKDistinctPerTenant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ks, err := encryption.NewKeyStoreLegacy("test-master-key-for-pbt-at-least-32chars")
		if err != nil {
			t.Fatalf("NewKeyStore: %v", err)
		}
		tenant1 := rapid.StringMatching(`[a-z]{3,16}`).Draw(t, "tenant1")
		tenant2 := rapid.StringMatching(`[a-z]{3,16}`).Draw(t, "tenant2")
		if tenant1 == tenant2 {
			tenant2 = tenant2 + "x"
		}

		dek1, err := ks.DeriveTenantDEK(tenant1)
		if err != nil {
			t.Fatalf("DeriveTenantDEK(%s): %v", tenant1, err)
		}
		dek2, err := ks.DeriveTenantDEK(tenant2)
		if err != nil {
			t.Fatalf("DeriveTenantDEK(%s): %v", tenant2, err)
		}

		if len(dek1) != 32 || len(dek2) != 32 {
			t.Fatalf("DEK length: got %d and %d, want 32", len(dek1), len(dek2))
		}

		var a, b [32]byte
		copy(a[:], dek1)
		copy(b[:], dek2)
		if a == b {
			t.Fatalf("tenants %q and %q got identical DEKs", tenant1, tenant2)
		}
	})
}

// Property: EncryptPlaintext/DecryptPlaintext round-trips with per-tenant DEK.
func TestPBT_DEKEncryptDecryptRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ks, err := encryption.NewKeyStoreLegacy("test-master-key-for-pbt-at-least-32chars")
		if err != nil {
			t.Fatalf("NewKeyStore: %v", err)
		}
		tenantID := rapid.StringMatching(`[a-z]{3,16}`).Draw(t, "tenantID")
		plaintext := rapid.SliceOf(rapid.Byte()).Draw(t, "plaintext")

		enc, err := ks.EncryptPlaintext(tenantID, plaintext)
		if err != nil {
			t.Fatalf("EncryptPlaintext: %v", err)
		}

		dec, err := ks.DecryptPlaintext(tenantID, enc)
		if err != nil {
			t.Fatalf("DecryptPlaintext: %v", err)
		}

		if len(dec) != len(plaintext) {
			t.Fatalf("length mismatch: got %d, want %d", len(dec), len(plaintext))
		}
		for i := range plaintext {
			if dec[i] != plaintext[i] {
				t.Fatalf("byte mismatch at index %d", i)
			}
		}
	})
}

// Property: DecryptPlaintext with wrong tenant fails (different DEK).
func TestPBT_DEKCrossTenantDecryptFails(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ks, err := encryption.NewKeyStoreLegacy("test-master-key-for-pbt-at-least-32chars")
		if err != nil {
			t.Fatalf("NewKeyStore: %v", err)
		}
		tenant1 := rapid.StringMatching(`[a-z]{3,16}`).Draw(t, "tenant1")
		tenant2 := rapid.StringMatching(`[a-z]{3,16}`).Draw(t, "tenant2")
		if tenant1 == tenant2 {
			tenant2 = tenant2 + "x"
		}

		enc, err := ks.EncryptPlaintext(tenant1, []byte("sensitive data"))
		if err != nil {
			t.Fatalf("EncryptPlaintext: %v", err)
		}

		_, err = ks.DecryptPlaintext(tenant2, enc)
		if err == nil {
			t.Fatalf("cross-tenant decryption should fail")
		}
	})
}

// Property: random 32-byte DEKs encrypt/decrypt correctly.
func TestPBT_RawDEKEncryptDecryptRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var dek [32]byte
		_, _ = rand.Read(dek[:])
		plaintext := rapid.SliceOf(rapid.Byte()).Draw(t, "plaintext")

		enc, err := encryption.EncryptValue(dek[:], plaintext)
		if err != nil {
			t.Fatalf("EncryptValue: %v", err)
		}

		dec, err := encryption.DecryptValue(dek[:], enc)
		if err != nil {
			t.Fatalf("DecryptValue: %v", err)
		}

		if len(dec) != len(plaintext) {
			t.Fatalf("length mismatch: got %d, want %d", len(dec), len(plaintext))
		}
		for i := range plaintext {
			if dec[i] != plaintext[i] {
				t.Fatalf("byte mismatch at index %d", i)
			}
		}
	})
}
