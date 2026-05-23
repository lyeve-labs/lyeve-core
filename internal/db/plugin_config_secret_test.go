package db

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSealer is a reversible stand-in for the key store. Encryption itself is
// covered in pkg/security/encryption. What matters here is which values reach
// the sealer and what the store does with the result.
type fakeSealer struct {
	failEncrypt bool
	failDecrypt bool
}

func (f fakeSealer) EncryptPlaintext(tenantID string, plaintext []byte) (string, error) {
	if f.failEncrypt {
		return "", errors.New("encrypt failed")
	}
	return tenantID + "|" + string(plaintext), nil
}

func (f fakeSealer) DecryptPlaintext(tenantID, enc string) ([]byte, error) {
	if f.failDecrypt {
		return nil, errors.New("decrypt failed")
	}
	prefix := tenantID + "|"
	if len(enc) < len(prefix) || enc[:len(prefix)] != prefix {
		return nil, errors.New("wrong tenant")
	}
	return []byte(enc[len(prefix):]), nil
}

func TestSealValues_EncryptsOnlyCredentials(t *testing.T) {
	got, preserve, err := sealValues(fakeSealer{}, "t1", map[string]any{
		"example_host": "mail.example.com",
		"example_pass": "hunter2",
		"timeout":      30.0,
	})
	require.NoError(t, err)
	assert.Empty(t, preserve)

	assert.Equal(t, "mail.example.com", got["example_host"], "an ordinary setting is stored as it stands")
	assert.Equal(t, 30.0, got["timeout"])
	assert.Equal(t, map[string]any{SecretEnvelopeField: "t1|hunter2"}, got["example_pass"])
}

func TestSealValues_CredentialArrivalModes(t *testing.T) {
	tests := []struct {
		name         string
		value        string
		wantStored   bool
		wantPreserve bool
	}{
		{name: "a value replaces what is stored", value: "new-key", wantStored: true},
		{name: "the mask leaves the stored value alone", value: SecretMask, wantPreserve: true},
		{name: "empty clears the credential", value: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, preserve, err := sealValues(fakeSealer{}, "t1", map[string]any{"api_key": tc.value})
			require.NoError(t, err)

			_, stored := got["api_key"]
			assert.Equal(t, tc.wantStored, stored, "stored")
			assert.Equal(t, tc.wantPreserve, len(preserve) == 1, "preserved")
		})
	}
}

func TestSealValues_RefusesCredentialWithoutASealer(t *testing.T) {
	// Writing it in plain text would put the credential in the clear in a table
	// the admin API can read.
	_, _, err := sealValues(nil, "t1", map[string]any{"api_key": "sk_live_x"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoSealer)
	assert.Contains(t, err.Error(), "api_key")
}

func TestSealValues_MaskWithoutASealerIsNotAnError(t *testing.T) {
	// Saving an unrelated setting must not fail just because a credential the
	// operator did not touch is already stored.
	_, preserve, err := sealValues(nil, "t1", map[string]any{
		"api_key":      SecretMask,
		"example_host": "mail.example.com",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"api_key"}, preserve)
}

func TestSealValues_RejectsNonTextCredential(t *testing.T) {
	_, _, err := sealValues(fakeSealer{}, "t1", map[string]any{"api_key": 12345})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be text")
}

func TestSealValues_ReportsEncryptFailure(t *testing.T) {
	_, _, err := sealValues(fakeSealer{failEncrypt: true}, "t1", map[string]any{"api_key": "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key")
}

func TestUnsealValues_RoundTrips(t *testing.T) {
	sealed, _, err := sealValues(fakeSealer{}, "t1", map[string]any{
		"example_host": "mail.example.com",
		"example_pass": "hunter2",
	})
	require.NoError(t, err)

	got, failed := unsealValues(fakeSealer{}, "t1", sealed)
	assert.Empty(t, failed)
	assert.Equal(t, "hunter2", got["example_pass"])
	assert.Equal(t, "mail.example.com", got["example_host"])
}

func TestUnsealValues_BareStringPassesThrough(t *testing.T) {
	// A credential stored as a bare string still has to reach the plugin that
	// owns it. The next save seals it.
	got, failed := unsealValues(fakeSealer{}, "t1", map[string]any{"api_key": "bare-plaintext"})
	assert.Empty(t, failed)
	assert.Equal(t, "bare-plaintext", got["api_key"])
}

func TestUnsealValues_UndecryptableIsWithheldAndNamed(t *testing.T) {
	// Handing a plugin ciphertext as though it were an API key fails at the far
	// end of an integration with nothing pointing back here.
	stored := map[string]any{
		"api_key":      map[string]any{SecretEnvelopeField: "t1|value"},
		"example_host": "mail.example.com",
	}

	got, failed := unsealValues(fakeSealer{failDecrypt: true}, "t1", stored)
	assert.Equal(t, []string{"api_key"}, failed)
	assert.NotContains(t, got, "api_key")
	assert.Equal(t, "mail.example.com", got["example_host"], "ordinary settings still load")
}

func TestUnsealValues_WithoutASealerWithholdsCiphertext(t *testing.T) {
	got, failed := unsealValues(nil, "t1", map[string]any{
		"api_key": map[string]any{SecretEnvelopeField: "t1|value"},
	})
	assert.Equal(t, []string{"api_key"}, failed)
	assert.NotContains(t, got, "api_key")
}

func TestUnsealValues_TenantScopesTheKey(t *testing.T) {
	// The derived key is per tenant, so one tenant's stored credential must not
	// decrypt under another's.
	sealed, _, err := sealValues(fakeSealer{}, "tenant-a", map[string]any{"api_key": "secret"})
	require.NoError(t, err)

	_, failed := unsealValues(fakeSealer{}, "tenant-b", sealed)
	assert.Equal(t, []string{"api_key"}, failed)
}

func TestMaskValues(t *testing.T) {
	got := maskValues(map[string]any{
		"example_host": "mail.example.com",
		"example_pass": map[string]any{SecretEnvelopeField: "t1|hunter2"},
		"bare_secret":  "stored-as-plain-text",
		"empty_token":  "",
		"timeout":      30.0,
	})

	assert.Equal(t, "mail.example.com", got["example_host"])
	assert.Equal(t, 30.0, got["timeout"])
	assert.Equal(t, SecretMask, got["example_pass"], "a sealed credential reads as the mask")
	assert.Equal(t, SecretMask, got["bare_secret"], "plain text in the table is still withheld")
	assert.NotContains(t, got, "empty_token", "an unset credential is absent, not masked")
}

func TestMaskValues_NeverLeaksCiphertext(t *testing.T) {
	got := maskValues(map[string]any{"api_key": map[string]any{SecretEnvelopeField: "t1|sk_live_x"}})
	assert.Equal(t, SecretMask, got["api_key"])
	assert.NotContains(t, got, SecretEnvelopeField)
}

func TestSecretEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		val      any
		wantEnc  string
		wantSeal bool
	}{
		{name: "a sealed value", val: map[string]any{SecretEnvelopeField: "abc"}, wantEnc: "abc", wantSeal: true},
		{name: "a plain string", val: "abc"},
		{name: "an empty envelope", val: map[string]any{SecretEnvelopeField: ""}},
		{name: "a wrong-typed envelope", val: map[string]any{SecretEnvelopeField: 7}},
		{name: "a map with other keys is a value, not an envelope",
			val: map[string]any{SecretEnvelopeField: "abc", "other": 1}},
		{name: "an ordinary nested object", val: map[string]any{"host": "x"}},
		{name: "nil", val: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc, sealed := secretEnvelope(tc.val)
			assert.Equal(t, tc.wantSeal, sealed)
			assert.Equal(t, tc.wantEnc, enc)
		})
	}
}

func TestNewConfigSealer(t *testing.T) {
	assert.Nil(t, NewConfigSealer(""), "no key means the store refuses to save credentials")

	sealer := NewConfigSealer("a-master-key-of-reasonable-length")
	require.NotNil(t, sealer)

	enc, err := sealer.EncryptPlaintext("t1", []byte("hunter2"))
	require.NoError(t, err)
	assert.NotContains(t, enc, "hunter2")

	plain, err := sealer.DecryptPlaintext("t1", enc)
	require.NoError(t, err)
	assert.Equal(t, "hunter2", string(plain))
}
