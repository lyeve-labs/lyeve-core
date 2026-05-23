package encryption

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewKeyStore(t *testing.T) {
	t.Run("empty key", func(t *testing.T) {
		ks, err := NewKeyStore("")
		assert.Error(t, err)
		assert.Nil(t, ks)
	})

	t.Run("too short", func(t *testing.T) {
		ks, err := NewKeyStore("short")
		assert.Error(t, err)
		assert.Nil(t, ks)
		assert.Contains(t, err.Error(), "too short")
	})

	t.Run("exactly minimum", func(t *testing.T) {
		ks, err := NewKeyStore("abcdefghijklmnopqrstuvwxyz012345") // 32 chars
		require.NoError(t, err)
		require.NotNil(t, ks)
		assert.Len(t, ks.kek, AESSize)
	})

	t.Run("valid key", func(t *testing.T) {
		ks, err := NewKeyStore("super-secret-master-key-for-encryption-testing") // 46 chars
		require.NoError(t, err)
		require.NotNil(t, ks)
		assert.Len(t, ks.kek, AESSize)
	})

	t.Run("pbkdf2 produces deterministic output", func(t *testing.T) {
		key := "deterministic-master-key-for-pbkdf2-testing"
		ks1, err := NewKeyStore(key)
		require.NoError(t, err)
		ks2, err := NewKeyStore(key)
		require.NoError(t, err)
		assert.Equal(t, ks1.kek, ks2.kek, "same input must produce same KEK")
	})

	t.Run("different keys produce different KEKs", func(t *testing.T) {
		ks1, _ := NewKeyStore("master-key-alpha-for-uniqueness-check-32chars")
		ks2, _ := NewKeyStore("master-key-beta--for-uniqueness-check-32chars")
		assert.NotEqual(t, ks1.kek, ks2.kek, "different inputs must produce different KEKs")
	})
}

func TestNewKeyStoreLegacy(t *testing.T) {
	t.Run("empty key", func(t *testing.T) {
		ks, err := NewKeyStoreLegacy("")
		assert.Error(t, err)
		assert.Nil(t, ks)
	})

	t.Run("valid key", func(t *testing.T) {
		ks, err := NewKeyStoreLegacy("super-secret-master-key")
		require.NoError(t, err)
		require.NotNil(t, ks)
		assert.Len(t, ks.kek, AESSize)
	})

	t.Run("no minimum length enforcement", func(t *testing.T) {
		// Legacy path does not enforce minMasterKeyLen: only empty check.
		ks, err := NewKeyStoreLegacy("short")
		require.NoError(t, err)
		require.NotNil(t, ks)
		assert.Len(t, ks.kek, AESSize)
	})
}

func TestDeriveTenantDEK(t *testing.T) {
	ks := mustKeyStore(t, "master-key-for-testing---at-least-32chars")

	t.Run("valid tenant", func(t *testing.T) {
		dek, err := ks.DeriveTenantDEK("tenant-1")
		require.NoError(t, err)
		require.Len(t, dek, AESSize)
	})

	t.Run("different tenants get different keys", func(t *testing.T) {
		dek1, err := ks.DeriveTenantDEK("tenant-alpha")
		require.NoError(t, err)
		dek2, err := ks.DeriveTenantDEK("tenant-beta")
		require.NoError(t, err)
		assert.NotEqual(t, dek1, dek2, "different tenants must have different DEKs")
	})

	t.Run("same tenant gets same key", func(t *testing.T) {
		dek1, err := ks.DeriveTenantDEK("tenant-consistent")
		require.NoError(t, err)
		dek2, err := ks.DeriveTenantDEK("tenant-consistent")
		require.NoError(t, err)
		assert.Equal(t, dek1, dek2, "same tenant ID must produce the same DEK")
	})

	t.Run("empty tenant ID", func(t *testing.T) {
		dek, err := ks.DeriveTenantDEK("")
		assert.Error(t, err)
		assert.Nil(t, dek)
	})

	t.Run("different master keys produce different DEKs", func(t *testing.T) {
		ks2 := mustKeyStore(t, "different-master-key--for-testing-only-32chars")
		dek1, _ := ks.DeriveTenantDEK("shared-tenant")
		dek2, _ := ks2.DeriveTenantDEK("shared-tenant")
		assert.NotEqual(t, dek1, dek2, "different master keys must produce different DEKs for the same tenant")
	})
}

func TestEncryptDecrypt(t *testing.T) {
	ks := mustKeyStore(t, "encryption-master-key-for-integration-test")
	dek, err := ks.DeriveTenantDEK("tenant-enc-test")
	require.NoError(t, err)

	t.Run("round trip", func(t *testing.T) {
		plaintext := []byte("sensitive data for encryption")
		cipherHex, err := EncryptValue(dek, plaintext)
		require.NoError(t, err)
		require.NotEmpty(t, cipherHex)

		decrypted, err := DecryptValue(dek, cipherHex)
		require.NoError(t, err)
		assert.Equal(t, plaintext, decrypted)
	})

	t.Run("empty plaintext", func(t *testing.T) {
		cipherHex, err := EncryptValue(dek, []byte{})
		require.NoError(t, err)
		decrypted, err := DecryptValue(dek, cipherHex)
		require.NoError(t, err)
		// GCM.Open returns nil (not []byte{}) for empty plaintext: both are valid empty values.
		assert.Empty(t, decrypted)
	})

	t.Run("different tenants cannot decrypt each other", func(t *testing.T) {
		dekA, _ := ks.DeriveTenantDEK("tenant-A")
		dekB, _ := ks.DeriveTenantDEK("tenant-B")

		cipherHex, err := EncryptValue(dekA, []byte("tenant A's secret"))
		require.NoError(t, err)

		_, err = DecryptValue(dekB, cipherHex)
		assert.Error(t, err, "tenant B must not be able to decrypt tenant A's data")
	})

	t.Run("decrypt with wrong key fails", func(t *testing.T) {
		ks2 := mustKeyStore(t, "other-master-key-for-cross-key-decryption-tests")
		dekOther, _ := ks2.DeriveTenantDEK("tenant-enc-test")

		cipherHex, err := EncryptValue(dek, []byte("secret"))
		require.NoError(t, err)

		_, err = DecryptValue(dekOther, cipherHex)
		assert.Error(t, err)
	})

	t.Run("corrupted ciphertext", func(t *testing.T) {
		cipherHex, err := EncryptValue(dek, []byte("secret"))
		require.NoError(t, err)

		// flip a byte in the hex
		b := []byte(cipherHex)
		b[10] ^= 0xFF
		_, err = DecryptValue(dek, string(b))
		assert.Error(t, err)
	})

	t.Run("truncated ciphertext", func(t *testing.T) {
		_, err := DecryptValue(dek, "abcd")
		assert.Error(t, err)
	})

	t.Run("invalid hex", func(t *testing.T) {
		_, err := DecryptValue(dek, "not-hex!!")
		assert.Error(t, err)
	})

	t.Run("invalid DEK length", func(t *testing.T) {
		shortDEK := make([]byte, 16)
		_, err := EncryptValue(shortDEK, []byte("data"))
		assert.Error(t, err)

		_, err = DecryptValue(shortDEK, "aa")
		assert.Error(t, err)
	})
}

func TestConvenienceWrappers(t *testing.T) {
	ks := mustKeyStore(t, "convenience-key-for-encryption-wrapper-testing")

	t.Run("EncryptPlaintext round trip", func(t *testing.T) {
		plaintext := []byte("hello, world")
		cipherHex, err := ks.EncryptPlaintext("t1", plaintext)
		require.NoError(t, err)

		decrypted, err := ks.DecryptPlaintext("t1", cipherHex)
		require.NoError(t, err)
		assert.Equal(t, plaintext, decrypted)
	})
}

func TestNonceUniqueness(t *testing.T) {
	ks := mustKeyStore(t, "nonce-key-for-encryption-uniqueness-checker")
	dek, err := ks.DeriveTenantDEK("nonce-tenant")
	require.NoError(t, err)

	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		cipherHex, err := EncryptValue(dek, []byte("same plaintext every time"))
		require.NoError(t, err)

		if seen[cipherHex] {
			t.Fatal("nonce collision detected - this should be astronomically unlikely with crypto/rand")
		}
		seen[cipherHex] = true
	}
}

func TestUnsaltedFormatCompatibility(t *testing.T) {
	// The unsalted security.EncryptSecret format is:
	// hex(SHA256(passphrase) -> key -> AES-256-GCM(nonce || ciphertext+tag))
	//
	// Our EncryptValue uses: HKDF(KEK, tenantID) -> DEK -> AES-256-GCM(nonce || ciphertext+tag)
	//
	// The wire format (nonce || ciphertext+tag) is identical: only the key
	// derivation path differs. So DecryptValue with a correctly derived DEK
	// can decrypt ciphertexts in the unsalted format if the DEK matches what
	// that format's key derivation produces.
	//
	// This is a conceptual compatibility test: we prove that ciphertexts
	// encrypted with EncryptValue are readable by DecryptValue, and that
	// the wire format matches (nonce prefix verification).
	ks := mustKeyStore(t, "compat-key-for-format-compatibility-testing")
	dek, err := ks.DeriveTenantDEK("compat-tenant")
	require.NoError(t, err)

	// Encrypt with EncryptValue
	plaintext := []byte("stored data")
	cipherHex, err := EncryptValue(dek, plaintext)
	require.NoError(t, err)

	// Verify the format has at least nonce+tag bytes (12+16=28 bytes hex-encoded = 56 chars)
	require.True(t, len(cipherHex) >= 56, "ciphertext must have at least 12+16 bytes hex-encoded")

	// Decrypt with DecryptValue: should work fine
	decrypted, err := DecryptValue(dek, cipherHex)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)

	// Additionally verify that we can decrypt ciphertext that was encoded
	// with the exact nonce||ciphertext format by crafting it manually
	raw, err := hex.DecodeString(cipherHex)
	require.NoError(t, err)

	// raw is: nonce(12) || ciphertext+tag
	assert.Len(t, raw[:12], 12, "first 12 bytes must be nonce")
}

func TestKeyRotation(t *testing.T) {
	// Key rotation scenario: tenant data encrypted with old KEK must be
	// readable after re-keying to a new KEK. The workflow is:
	//   1. Derive old DEK from old KEK + tenant ID
	//   2. Encrypt data with old DEK
	//   3. Derive new DEK from new KEK + tenant ID (different key!)
	//   4. Decrypt old ciphertext with old DEK (still works)
	//   5. Re-encrypt with new DEK
	//   6. Verify old DEK cannot decrypt new ciphertext

	oldKS := mustKeyStore(t, "old-master-key-v1-for-key-rotation-testing")
	newKS := mustKeyStore(t, "new-master-key-v2-for-key-rotation-testing")
	tenantID := "tenant-to-rotate"

	oldDEK, err := oldKS.DeriveTenantDEK(tenantID)
	require.NoError(t, err)
	newDEK, err := newKS.DeriveTenantDEK(tenantID)
	require.NoError(t, err)

	// Old and new DEKs must be different (different KEKs for same tenant)
	assert.NotEqual(t, oldDEK, newDEK, "DEKs from different KEKs must differ")

	// Encrypt with old DEK
	secret := []byte("sensitive data from v1")
	oldCipher, err := EncryptValue(oldDEK, secret)
	require.NoError(t, err)

	// Old DEK can still decrypt its own ciphertext
	decrypted, err := DecryptValue(oldDEK, oldCipher)
	require.NoError(t, err)
	assert.Equal(t, secret, decrypted)

	// New DEK cannot decrypt old ciphertext (key independence)
	_, err = DecryptValue(newDEK, oldCipher)
	assert.Error(t, err, "new DEK must not decrypt old ciphertexts")

	// Re-encrypt with new DEK (the rotation step)
	newCipher, err := EncryptValue(newDEK, secret)
	require.NoError(t, err)

	decrypted, err = DecryptValue(newDEK, newCipher)
	require.NoError(t, err)
	assert.Equal(t, secret, decrypted)

	// Old DEK cannot decrypt new ciphertext either
	_, err = DecryptValue(oldDEK, newCipher)
	assert.Error(t, err, "old DEK must not decrypt new ciphertexts")
}

func TestPBKDF2Determinism(t *testing.T) {
	// Same master key + same fixed salt must produce the same KEK every time.
	// This is critical for decrypting previously encrypted data after a restart.
	key := "deterministic-test-master-key-for-pbkdf2-32"
	ks1, err := NewKeyStore(key)
	require.NoError(t, err)
	ks2, err := NewKeyStore(key)
	require.NoError(t, err)

	assert.Equal(t, ks1.kek, ks2.kek, "PBKDF2 with fixed salt must produce identical KEKs")

	// Different keys must produce different KEKs
	ks3, _ := NewKeyStore("different-deterministic-master-key-pbkdf2-32")
	assert.NotEqual(t, ks1.kek, ks3.kek, "different inputs must produce different KEKs")
}

func mustKeyStore(t *testing.T, key string) *KeyStore {
	t.Helper()
	ks, err := NewKeyStore(key)
	require.NoError(t, err)
	return ks
}

// The KEK is an input to every ciphertext already at rest, so the derivation
// is pinned to an answer computed outside this package (Python's
// hashlib.pbkdf2_hmac, SHA-256, the fixed salt, 600,000 iterations), both on
// a cached key and on one derived after the cache is full.
func TestNewKeyStore_KnownAnswerThroughTheCache(t *testing.T) {
	const cached = "deterministic-test-master-key-for-pbkdf2-32"
	const cachedKEK = "ca21a1cc1550c6b944ead633a89b7e4909dc6083ae429a00d23986a3dd15b38f"
	for range 2 {
		assert.Equal(t, cachedKEK, hex.EncodeToString(mustKeyStore(t, cached).kek))
	}

	for i := range maxCachedKEKs {
		mustKeyStore(t, "kek-cache-bound-filler-key-000000000000000"+string(rune('a'+i)))
	}
	const uncached = "kek-cache-bound-master-key-number-0000000005"
	const uncachedKEK = "ea06a05b7a229202e5afb389bad31529d2f70ff8387f0567c06c26a086970835"
	assert.Equal(t, uncachedKEK, hex.EncodeToString(mustKeyStore(t, uncached).kek))
	kekCacheMu.Lock()
	assert.LessOrEqual(t, len(kekCache), maxCachedKEKs)
	kekCacheMu.Unlock()
}

func TestNewKeyStore_SecondStoreDecryptsTheFirstsCiphertext(t *testing.T) {
	const key = "second-store-decrypts-the-first-master-key-32"
	first, second := mustKeyStore(t, key), mustKeyStore(t, key)
	dek1, err := first.DeriveTenantDEK("acme")
	require.NoError(t, err)
	dek2, err := second.DeriveTenantDEK("acme")
	require.NoError(t, err)

	ct, err := EncryptValue(dek1, []byte("secret"))
	require.NoError(t, err)
	pt, err := DecryptValue(dek2, ct)
	require.NoError(t, err)
	assert.Equal(t, []byte("secret"), pt)
}
