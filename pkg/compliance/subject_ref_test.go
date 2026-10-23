package compliance

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const knownAddress = "subject@example.com"

// keepSubjectRefKey puts back the key the process held before the test, so a
// test that installs its own leaves no trace on the next one.
func keepSubjectRefKey(t *testing.T) {
	t.Helper()
	saved := subjectRefKey.Load()
	t.Cleanup(func() { subjectRefKey.Store(saved) })
}

func TestSubjectRef_KnownAddressDoesNotMatchItsDigest(t *testing.T) {
	keepSubjectRefKey(t)
	sum := sha256.Sum256([]byte(knownAddress))
	digestPrefix := hex.EncodeToString(sum[:6])

	ref := SubjectRef(knownAddress)
	assert.Len(t, ref, 12)
	assert.NotEqual(t, digestPrefix, ref,
		"before any key is installed, the reference must not be the SHA-256 prefix anyone holding the address can compute")

	require.NoError(t, SetSubjectRefSecret([]byte("install-secret-for-the-subject-reference-test")))
	ref = SubjectRef(knownAddress)
	assert.Len(t, ref, 12)
	assert.NotEqual(t, digestPrefix, ref,
		"under an install key, the reference must not be the SHA-256 prefix anyone holding the address can compute")
}

func TestSubjectRef_OneKeyGivesAStableReference(t *testing.T) {
	keepSubjectRefKey(t)
	secret := []byte("install-secret-for-the-subject-reference-test")

	require.NoError(t, SetSubjectRefSecret(secret))
	first := SubjectRef(knownAddress)
	require.NoError(t, SetSubjectRefSecret([]byte("another-install-secret")))
	require.NoError(t, SetSubjectRefSecret(secret))

	assert.Equal(t, first, SubjectRef(knownAddress), "one secret must give one reference, or lines written across restarts stop correlating")
	// HMAC-SHA-256 under HKDF-SHA-256(secret, info "lyeve subject reference
	// key v1"), computed outside Go. A change here changes every reference an
	// install has already logged.
	assert.Equal(t, "34b9c8122332", first)
}

func TestSubjectRef_TwoKeysGiveDifferentReferences(t *testing.T) {
	keepSubjectRefKey(t)

	require.NoError(t, SetSubjectRefSecret([]byte("first-install-secret")))
	first := SubjectRef(knownAddress)
	require.NoError(t, SetSubjectRefSecret([]byte("second-install-secret")))

	assert.NotEqual(t, first, SubjectRef(knownAddress))
}

func TestSetSubjectRefSecret_RefusesAnEmptySecret(t *testing.T) {
	keepSubjectRefKey(t)
	before := SubjectRef(knownAddress)

	require.Error(t, SetSubjectRefSecret(nil))
	assert.Equal(t, before, SubjectRef(knownAddress), "a refused secret must leave the key as it was")
}
