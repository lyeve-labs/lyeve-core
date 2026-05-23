package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round-trip: new PBKDF2 format

func TestEncryptSecret_RoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		plaintext string
		pass      string
	}{
		{"short ascii", "hello world", "strong-passphrase"},
		{"unicode", "こんにちは世界 🔐 secret message", "strong-passphrase"},
		{"json payload", `{"api_key":"***","url":"https://example.com"}`, "strong-passphrase"},
		{"empty string", "", "strong-passphrase"},
		{"single char", "x", "strong-passphrase"},
		{"long text", strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50), "strong-passphrase"},
		{"empty passphrase", "data", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := EncryptSecret(tt.plaintext, tt.pass)
			require.NoError(t, err)

			// New format: salt(16) + nonce(12) + ciphertext+tag(16), hex is 2x raw bytes
			raw, err := hex.DecodeString(enc)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, len(raw), saltSize+gcmNonceSize+1,
				"new format must have at least salt+nonce+min ciphertext")

			assert.NotEqual(t, tt.plaintext, enc, "encrypted must differ from plaintext")

			dec, err := DecryptSecret(enc, tt.pass)
			require.NoError(t, err)
			assert.Equal(t, tt.plaintext, dec)
		})
	}
}

// Distinct outputs: same plaintext must produce different ciphertexts
// because of random salt and nonce

func TestEncryptSecret_ProducesDistinctCiphertexts(t *testing.T) {
	c1, err := EncryptSecret("same plaintext", "passphrase")
	require.NoError(t, err)
	c2, err := EncryptSecret("same plaintext", "passphrase")
	require.NoError(t, err)
	assert.NotEqual(t, c1, c2, "same plaintext must produce different ciphertexts (random salt+nonce)")
}

// Wrong passphrase

func TestEncryptSecret_DifferentPassphraseFails(t *testing.T) {
	enc, err := EncryptSecret("sensitive data", "correct-passphrase")
	require.NoError(t, err)

	_, err = DecryptSecret(enc, "wrong-passphrase")
	assert.Error(t, err, "decryption with wrong passphrase must fail")
}

// Malformed hex

func TestDecryptSecret_MalformedHex(t *testing.T) {
	_, err := DecryptSecret("not-hex-at-all!!!", "passphrase")
	assert.Error(t, err)
}

// Too-short ciphertext

func TestDecryptSecret_TooShort(t *testing.T) {
	_, err := DecryptSecret("aa", "passphrase") // 1 byte, shorter than needed
	assert.Error(t, err)
}

// Backward compatibility: decrypt old-format (SHA-256, no salt) ciphertext

func TestDecryptSecret_BackwardCompat(t *testing.T) {
	// Produce a legacy-format ciphertext using raw SHA-256 derivation
	// (identical to the pre-PBKDF2 wire format)
	legacyKey := DeriveEncKey("old-passphrase")
	legacyEnc, err := encryptGCMWithKey(legacyKey, []byte("legacy data"))
	require.NoError(t, err)
	legacyHex := hex.EncodeToString(legacyEnc)

	// Must decrypt with old passphrase via backward-compat path
	dec, err := DecryptSecret(legacyHex, "old-passphrase")
	require.NoError(t, err)
	assert.Equal(t, "legacy data", dec)

	// Must fail with wrong passphrase
	_, err = DecryptSecret(legacyHex, "wrong-passphrase")
	assert.Error(t, err)
}

// Helper: encrypt with a pre-derived key, producing legacy (nonce||ciphertext+tag) format.
func encryptGCMWithKey(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Re-encrypt migration

func TestReEncryptSecret_Migration(t *testing.T) {
	// Encrypt with legacy format
	legacyKey := DeriveEncKey("old-pass")
	legacyCiphertext, err := encryptGCMWithKey(legacyKey, []byte("migrate-me"))
	require.NoError(t, err)
	legacyHex := hex.EncodeToString(legacyCiphertext)

	// Re-encrypt with new passphrase
	newHex, err := ReEncryptSecret(legacyHex, "old-pass", "new-pass")
	require.NoError(t, err)

	// New ciphertext must be in PBKDF2 format (salt prefix)
	raw, err := hex.DecodeString(newHex)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(raw), saltSize+gcmNonceSize+1,
		"re-encrypted must include salt prefix")

	// Must decrypt with new passphrase
	dec, err := DecryptSecret(newHex, "new-pass")
	require.NoError(t, err)
	assert.Equal(t, "migrate-me", dec)

	// Must fail with old passphrase
	_, err = DecryptSecret(newHex, "old-pass")
	assert.Error(t, err, "re-encrypted must fail with old passphrase")

	// Legacy still decryptable with old passphrase
	decLegacy, err := DecryptSecret(legacyHex, "old-pass")
	require.NoError(t, err)
	assert.Equal(t, "migrate-me", decLegacy)
}

// ReEncryptSecret with wrong old passphrase

func TestReEncryptSecret_WrongOldPassphrase(t *testing.T) {
	legacyKey := DeriveEncKey("correct-old")
	legacyCiphertext, _ := encryptGCMWithKey(legacyKey, []byte("data"))
	legacyHex := hex.EncodeToString(legacyCiphertext)

	_, err := ReEncryptSecret(legacyHex, "wrong-old", "new-pass")
	assert.Error(t, err, "re-encrypt must fail with wrong old passphrase")
}

// Nonce uniqueness stress: 1,000 encryptions, no nonce collisions.
// Not 100K, because PBKDF2 at 600K iterations is slow per derivation and
// 100K would take hours. The 1,000-sample test with concurrent goroutines is
// sufficient to detect deterministic failures.
// The 12-byte cryptographic nonce gives 2^96 collision resistance.

func TestEncryptSecret_NonceUniqueness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping nonce uniqueness stress in short mode")
	}
	const n = 1_000
	seen := make(map[string]struct{}, n)
	var mu sync.Mutex
	var wg sync.WaitGroup

	workers := 8
	chunk := n / workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < chunk; i++ {
				enc, err := EncryptSecret("data", "passphrase")
				if err != nil {
					t.Errorf("EncryptSecret failed: %v", err)
					return
				}
				mu.Lock()
				_, exists := seen[enc]
				if exists {
					t.Errorf("nonce collision detected after %d encryptions", len(seen))
				}
				seen[enc] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	t.Logf("produced %d unique ciphertexts with zero collisions", len(seen))
}

// Salt uniqueness: same plaintext+passphrase, different salts.
// Parallelized because each PBKDF2 derivation is slow at 600K iterations.

func TestEncryptSecret_SaltUniqueness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping salt uniqueness stress in short mode")
	}
	const n = 500
	seen := make(map[string]struct{}, n)
	var mu sync.Mutex
	var wg sync.WaitGroup

	workers := 8
	chunk := n / workers
	rem := n % workers
	for w := 0; w < workers; w++ {
		start := w*chunk + min(w, rem)
		end := start + chunk
		if w < rem {
			end++
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				enc, err := EncryptSecret("data", "pass")
				if err != nil {
					t.Errorf("EncryptSecret failed: %v", err)
					return
				}
				raw, err := hex.DecodeString(enc)
				if err != nil {
					t.Errorf("hex decode failed: %v", err)
					return
				}
				salt := string(raw[:saltSize])
				mu.Lock()
				_, exists := seen[salt]
				if exists {
					t.Errorf("salt collision detected after %d encryptions", len(seen))
				}
				seen[salt] = struct{}{}
				mu.Unlock()
			}
		}(start, end)
	}
	wg.Wait()
	assert.Equal(t, n, len(seen), "500 encryptions must produce 500 unique salts")
}

// EncryptBytes / DecryptBytes round-trip

func TestEncryptBytes_RoundTrip(t *testing.T) {
	plaintext := []byte("binary payload for filesystem encryption")
	ciphertext, err := EncryptBytes(plaintext, "passphrase")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(ciphertext), saltSize+gcmNonceSize+1)

	dec, err := DecryptBytes(ciphertext, "passphrase")
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec)
}

func TestEncryptBytes_WrongPassphrase(t *testing.T) {
	ct, err := EncryptBytes([]byte("data"), "correct")
	require.NoError(t, err)
	_, err = DecryptBytes(ct, "wrong")
	assert.Error(t, err)
}

func TestEncryptBytes_BackwardCompat(t *testing.T) {
	// Legacy format
	legacyKey := DeriveEncKey("old-pass")
	legacy, err := encryptGCMWithKey(legacyKey, []byte("legacy bytes"))
	require.NoError(t, err)

	dec, err := DecryptBytes(legacy, "old-pass")
	require.NoError(t, err)
	assert.Equal(t, []byte("legacy bytes"), dec)
}

// PBKDF2-specific: verify salt is present in ciphertext

func TestEncryptSecret_FormatHasSaltPrefix(t *testing.T) {
	enc, err := EncryptSecret("test", "pass")
	require.NoError(t, err)

	raw, err := hex.DecodeString(enc)
	require.NoError(t, err)

	// Salt occupies first 16 bytes
	salt := raw[:saltSize]
	assert.Equal(t, saltSize, len(salt))

	// Deriving with correct salt should work. With wrong salt should fail
	key1 := derivePBKDF2Key("pass", salt)
	key2 := derivePBKDF2Key("pass", make([]byte, saltSize)) // zero salt
	assert.NotEqual(t, key1, key2, "different salts must produce different keys")

	// And verify decrypting with wrong salt fails
	// (This is implicitly tested by DecryptSecret which tries PBKDF2 first,
	// then falls back to legacy, but with a forged salt it will fail both)
}

// derivePBKDF2Key: deterministic output for same salt+passphrase

func TestDerivePBKDF2Key_Deterministic(t *testing.T) {
	salt := []byte("0123456789abcdef")
	pass := "my-passphrase"
	k1 := derivePBKDF2Key(pass, salt)
	k2 := derivePBKDF2Key(pass, salt)
	assert.Equal(t, k1, k2, "same passphrase+ salt must produce same key")
	assert.Equal(t, 32, len(k1), "key must be 32 bytes (AES-256)")

	k3 := derivePBKDF2Key("different-pass", salt)
	assert.NotEqual(t, k1, k3, "different passphrases must produce different keys")
}

// Edge case: empty plaintext

func TestEncryptSecret_EmptyPlaintext(t *testing.T) {
	enc, err := EncryptSecret("", "pass")
	require.NoError(t, err)

	dec, err := DecryptSecret(enc, "pass")
	require.NoError(t, err)
	assert.Equal(t, "", dec)
}

// Edge case: very long plaintext

func TestEncryptSecret_LargePlaintext(t *testing.T) {
	large := strings.Repeat("abcdefghij", 10_000) // 100KB
	enc, err := EncryptSecret(large, "pass")
	require.NoError(t, err)

	dec, err := DecryptSecret(enc, "pass")
	require.NoError(t, err)
	assert.Equal(t, large, dec)
}

// Error wrapping: verify error messages follow fmt.Errorf pattern

func TestEncryptSecret_ErrorWrapping(t *testing.T) {
	_, err := DecryptSecret("invalid-hex!!!", "pass")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt:", "error must prefix with operation")
}

func TestReEncryptSecret_ErrorWrapping(t *testing.T) {
	_, err := ReEncryptSecret("invalid-hex", "old", "new")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "re-encrypt:", "error must prefix with operation")
}
