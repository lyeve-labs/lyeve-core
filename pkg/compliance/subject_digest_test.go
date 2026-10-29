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

	// HMAC-SHA-256 under the key above of the label, a zero byte and the
	// address, computed outside Go. A change here leaves every entry an
	// install has written unfindable.
	assert.Equal(t, "hmac-sha256:d572c0156e00af3a9414b2aa26eb2d80e108d2b40fb8fe68fed70d63f98e8112", SubjectDigest(knownAddress))
}

// The log reference and the audit digest share the install key. Were the
// digest the same HMAC, a log line naming an account beside its reference
// would tie the account to its audit entries without the key.
func TestSubjectDigest_IsNotTheLogReference(t *testing.T) {
	keepSubjectRefKey(t)
	require.NoError(t, SetSubjectRefKey([]byte("install-key-for-the-subject-digest-test")))

	for _, identifier := range []string{knownAddress, "6f1c2a8e-3b4d-4e5f-8a9b-0c1d2e3f4a5b", "203.0.113.9"} {
		digest := strings.TrimPrefix(SubjectDigest(identifier), "hmac-sha256:")
		assert.False(t, strings.HasPrefix(digest, SubjectRef(identifier)), "%s: the digest starts with the log reference", identifier)
	}
}

// Each identifier is digested in the form the engine stores it in, so a
// request spelled another way names the same subject as the stored account:
// an address folded, an account id and an IP address in their canonical text.
func TestSubjectDigest_DigestsTheStoredForm(t *testing.T) {
	keepSubjectRefKey(t)
	require.NoError(t, SetSubjectRefKey([]byte("install-key-for-the-subject-digest-test")))

	const id = "6f1c2a8e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"
	for stored, spellings := range map[string][]string{
		knownAddress:  {"Subject@Example.COM", "SUBJECT@EXAMPLE.COM", " subject@example.com "},
		id:            {strings.ToUpper(id), "{" + id + "}", "urn:uuid:" + id},
		"2001:db8::1": {"2001:DB8::1", "2001:0db8:0000:0000:0000:0000:0000:0001"},
		"192.0.2.1":   {"::ffff:192.0.2.1"},
	} {
		for _, typed := range spellings {
			assert.Equal(t, SubjectDigest(stored), SubjectDigest(typed), "%s is %s", typed, stored)
		}
	}
	assert.NotEqual(t, SubjectDigest("Jane"), SubjectDigest("jane"), "a word that names no subject is digested as given")
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

	const id = "6f1c2a8e-3b4d-4e5f-8a9b-0c1d2e3f4a5b"
	got = SubjectDigests(strings.ToUpper(id))
	assert.Equal(t, SubjectDigest(id), got[0])
	assert.Contains(t, got, unkeyedDigest(strings.ToUpper(id)), "an old entry for the id as it was typed")
	assert.Contains(t, got, unkeyedDigest(id), "an old entry for the stored id")
}
