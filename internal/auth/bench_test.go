package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// JWT Signing benchmarks: EdDSA and HMAC paths

var (
	benchSecret = "this-is-a-32-byte-secret-key!!"
	benchUserID = uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	benchEmail  = "bench@example.com"
	benchRoles  = []string{"admin", "editor"}
)

func BenchmarkSign_EdDSA(b *testing.B) {
	if !auth.IsEdDSAActive() {
		b.Skip("EdDSA signing key not active - run InitJWTSigning first")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.Sign(benchSecret, 3600, benchUserID, benchEmail, benchRoles, "", 1)
		if err != nil {
			b.Fatalf("Sign: %v", err)
		}
	}
}

// Sign picks EdDSA when active, HMAC when inactive. This captures whichever path is live.
func BenchmarkSign_HMAC(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.Sign(benchSecret, 3600, benchUserID, benchEmail, benchRoles, "", 1)
		if err != nil {
			b.Fatalf("Sign: %v", err)
		}
	}
}

// EdDSA when active, HMAC otherwise.
func BenchmarkParse(b *testing.B) {
	token, err := auth.Sign(benchSecret, 3600, benchUserID, benchEmail, benchRoles, "", 1)
	if err != nil {
		b.Fatalf("setup Sign: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.Parse(benchSecret, token)
		if err != nil {
			b.Fatalf("Parse: %v", err)
		}
	}
}

func BenchmarkSignChallenge(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.SignChallenge(benchSecret, benchUserID, benchEmail, benchRoles, "")
		if err != nil {
			b.Fatalf("SignChallenge: %v", err)
		}
	}
}

func BenchmarkParseMulti_1Secret(b *testing.B) {
	token, err := auth.Sign(benchSecret, 3600, benchUserID, benchEmail, benchRoles, "", 1)
	if err != nil {
		b.Fatalf("setup Sign: %v", err)
	}
	secrets := []string{benchSecret}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.ParseMulti(secrets, token)
		if err != nil {
			b.Fatalf("ParseMulti: %v", err)
		}
	}
}

// Key rotation scenario.
func BenchmarkParseMulti_3Secrets(b *testing.B) {
	token, err := auth.Sign(benchSecret, 3600, benchUserID, benchEmail, benchRoles, "", 1)
	if err != nil {
		b.Fatalf("setup Sign: %v", err)
	}
	secrets := []string{"wrong-one", benchSecret, "wrong-two"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.ParseMulti(secrets, token)
		if err != nil {
			b.Fatalf("ParseMulti: %v", err)
		}
	}
}

// Password hashing benchmarks

var benchPassword = "correct-horse-battery-staple-2024!"

func BenchmarkHashPassword_Bcrypt(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.HashPassword("bcrypt", benchPassword)
		if err != nil {
			b.Fatalf("HashPassword(bcrypt): %v", err)
		}
	}
}

func BenchmarkHashPassword_Argon2id(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.HashPassword("argon2id", benchPassword)
		if err != nil {
			b.Fatalf("HashPassword(argon2id): %v", err)
		}
	}
}

func BenchmarkVerifyPassword_Bcrypt(b *testing.B) {
	hash, err := auth.HashPassword("bcrypt", benchPassword)
	if err != nil {
		b.Fatalf("setup HashPassword: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := auth.VerifyPassword("", hash, benchPassword); err != nil {
			b.Fatalf("VerifyPassword: %v", err)
		}
	}
}

func BenchmarkVerifyPassword_Argon2id(b *testing.B) {
	hash, err := auth.HashPassword("argon2id", benchPassword)
	if err != nil {
		b.Fatalf("setup HashPassword: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := auth.VerifyPassword("", hash, benchPassword); err != nil {
			b.Fatalf("VerifyPassword: %v", err)
		}
	}
}

// AES-256-GCM encryption benchmarks

var benchPassphrase = "a-strong-passphrase-for-benchmarking"
var benchPlaintext = "this is a secret value that needs encryption at rest with AES-256-GCM"

func BenchmarkEncryptSecret(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.EncryptSecret(benchPlaintext, benchPassphrase)
		if err != nil {
			b.Fatalf("EncryptSecret: %v", err)
		}
	}
}

func BenchmarkDecryptSecret(b *testing.B) {
	enc, err := auth.EncryptSecret(benchPlaintext, benchPassphrase)
	if err != nil {
		b.Fatalf("setup EncryptSecret: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, err := auth.DecryptSecret(enc, benchPassphrase)
		if err != nil {
			b.Fatalf("DecryptSecret: %v", err)
		}
	}
}

func BenchmarkEncryptDecrypt_RoundTrip(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		enc, err := auth.EncryptSecret(benchPlaintext, benchPassphrase)
		if err != nil {
			b.Fatalf("EncryptSecret: %v", err)
		}
		_, err = auth.DecryptSecret(enc, benchPassphrase)
		if err != nil {
			b.Fatalf("DecryptSecret: %v", err)
		}
	}
}

// Ed25519 key generation benchmark

func BenchmarkEd25519KeyGen(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			b.Fatalf("GenerateKey: %v", err)
		}
	}
}

func BenchmarkEd25519Sign(b *testing.B) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	msg := []byte(fmt.Sprintf("benchmark-message-%d", b.N))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = ed25519.Sign(priv, msg)
	}
	_ = pub
}

func BenchmarkEd25519Verify(b *testing.B) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	msg := []byte(fmt.Sprintf("benchmark-message-%d", b.N))
	sig := ed25519.Sign(priv, msg)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if !ed25519.Verify(pub, msg, sig) {
			b.Fatal("verify failed")
		}
	}
}
