package compliance

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unkeyedDigest is the digest anyone holding an address can compute, and the
// one entries written before the digest was keyed carry.
func unkeyedDigest(identifier string) string {
	sum := sha256.Sum256([]byte(identifier))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// The audit log outlives the erasure it records, so whoever reads it, a backup
// or an export must not be able to test a guessed address against it.
func TestSubjectDigest_AGuessHashedWithoutTheKeyMatchesNothing(t *testing.T) {
	keepSubjectRefKey(t)
	require.NoError(t, SetSubjectRefKey([]byte("install-key-for-the-subject-digest-test")))

	digest := SubjectDigest(knownAddress)
	assert.True(t, strings.HasPrefix(digest, "hmac-sha256:"), digest)
	assert.Len(t, strings.TrimPrefix(digest, "hmac-sha256:"), 64, "the digest is the whole HMAC, not a reference prefix")
	guess := unkeyedDigest(knownAddress)
	assert.NotEqual(t, guess, digest)
	assert.NotContains(t, digest, strings.TrimPrefix(guess, "sha256:"))

	require.NoError(t, SetSubjectRefKey([]byte("another-install-key")))
	assert.NotEqual(t, digest, SubjectDigest(knownAddress), "the digest depends on the install's key")
}

func TestSubjectDigest_OneKeyGivesAStableDigest(t *testing.T) {
	keepSubjectRefKey(t)
	key, err := hex.DecodeString("ebf2e080ad9e24fb12a35c603ccbc0c1af12fbdb3fa2f61d99572b3a2c06a057")
	require.NoError(t, err)
	require.NoError(t, SetSubjectRefKey(key))

	// HMAC-SHA-256 under the key above, computed outside Go. A change here
	// leaves every entry an install has written unfindable.
	assert.Equal(t, "hmac-sha256:34b9c8122332d277c72ad799d27ece5e9edf7c572ed1e5b6aa04c3f0bd465c16", SubjectDigest(knownAddress))
}

// An address is digested in the form the engine stores it in, so a request
// typed with other capitals names the same subject as the stored address.
// Any other identifier is digested as given.
func TestSubjectDigest_FoldsAnAddressOnly(t *testing.T) {
	keepSubjectRefKey(t)
	require.NoError(t, SetSubjectRefKey([]byte("install-key-for-the-subject-digest-test")))

	for _, typed := range []string{"Subject@Example.COM", "SUBJECT@EXAMPLE.COM", " subject@example.com "} {
		assert.Equal(t, SubjectDigest(knownAddress), SubjectDigest(typed), typed)
	}
	const id = "6f1c2a8e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"
	assert.NotEqual(t, SubjectDigest(id), SubjectDigest(strings.ToUpper(id)), "an account id is digested as given")
}

// The lookup has to find entries written before the digest was keyed: a DSAR
// entry digested the identifier as the request typed it, and a refused
// sign-in digested the folded address.
func TestSubjectDigests_FindEveryFormAnEntryWasWrittenIn(t *testing.T) {
	keepSubjectRefKey(t)
	require.NoError(t, SetSubjectRefKey([]byte("install-key-for-the-subject-digest-test")))

	const typed = "Subject@Example.COM"
	got := SubjectDigests(typed)
	require.NotEmpty(t, got)
	assert.Equal(t, SubjectDigest(knownAddress), got[0], "the current form comes first")
	assert.Contains(t, got, unkeyedDigest(typed), "an old entry for the address as it was typed")
	assert.Contains(t, got, unkeyedDigest(knownAddress), "an old entry for the stored address")
	assert.Len(t, got, 3)

	assert.Equal(t, []string{SubjectDigest(knownAddress), unkeyedDigest(knownAddress)}, SubjectDigests(knownAddress),
		"a form that repeats is listed once")
}
