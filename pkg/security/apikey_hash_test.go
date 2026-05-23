package security

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// withPepper installs a pepper for the duration of a test and restores the
// unpeppered (legacy) state afterwards, so global pepper state never leaks
// between tests in this package.
func withPepper(t *testing.T, pepper string) {
	t.Helper()
	SetAPIKeyPepper([]byte(pepper))
	t.Cleanup(func() { SetAPIKeyPepper(nil) })
}

func TestHashKeyPeppered_UnpepperedEqualsHashKey(t *testing.T) {
	// Default (no pepper installed) must be byte-for-byte identical to HashKey
	// so existing deployments are unaffected until they opt in.
	if APIKeyPepperConfigured() {
		t.Fatal("precondition: no pepper should be configured by default")
	}
	const raw = "ly_deadbeef"
	if got, want := HashKeyPeppered(raw), HashKey(raw); got != want {
		t.Fatalf("unpeppered HashKeyPeppered(%q) = %s, want %s", raw, got, want)
	}
	// And HashKey itself is plain SHA-256.
	sum := sha256.Sum256([]byte(raw))
	if HashKey(raw) != hex.EncodeToString(sum[:]) {
		t.Fatalf("HashKey is not plain SHA-256")
	}
}

func TestHashKeyPeppered_DiffersWhenPeppered(t *testing.T) {
	const raw = "ly_deadbeef"
	plain := HashKey(raw)

	withPepper(t, "pepper-A")
	if !APIKeyPepperConfigured() {
		t.Fatal("APIKeyPepperConfigured() = false after SetAPIKeyPepper")
	}
	peppered := HashKeyPeppered(raw)

	if peppered == plain {
		t.Fatal("peppered hash equals plain SHA-256 - HMAC not applied")
	}
	if len(peppered) != 64 {
		t.Fatalf("peppered hash length = %d, want 64 hex chars", len(peppered))
	}
	if _, err := hex.DecodeString(peppered); err != nil {
		t.Fatalf("peppered hash is not valid hex: %v", err)
	}
	// Deterministic for the same (pepper, raw).
	if HashKeyPeppered(raw) != peppered {
		t.Fatal("HashKeyPeppered is not deterministic")
	}
}

func TestHashKeyPeppered_DifferentPepperDifferentHash(t *testing.T) {
	const raw = "ly_deadbeef"

	SetAPIKeyPepper([]byte("pepper-A"))
	a := HashKeyPeppered(raw)
	SetAPIKeyPepper([]byte("pepper-B"))
	b := HashKeyPeppered(raw)
	t.Cleanup(func() { SetAPIKeyPepper(nil) })

	if a == b {
		t.Fatal("different peppers produced the same hash")
	}
}

func TestSetAPIKeyPepper_ClearAndCopy(t *testing.T) {
	buf := []byte("mutable-pepper")
	SetAPIKeyPepper(buf)
	before := HashKeyPeppered("ly_x")

	// Caller mutating its buffer after the call must not change our pepper  --
	// SetAPIKeyPepper keeps a defensive copy.
	for i := range buf {
		buf[i] = 0
	}
	if after := HashKeyPeppered("ly_x"); after != before {
		t.Fatal("pepper changed after caller mutated its buffer - copy not retained")
	}

	// Clearing reverts to legacy behavior.
	SetAPIKeyPepper(nil)
	if APIKeyPepperConfigured() {
		t.Fatal("APIKeyPepperConfigured() = true after clearing")
	}
	if HashKeyPeppered("ly_x") != HashKey("ly_x") {
		t.Fatal("cleared pepper did not revert to HashKey")
	}
}

func TestGenerateKey_UsesPepperedHash(t *testing.T) {
	withPepper(t, "pepper-gen")

	raw, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	if !strings.HasPrefix(raw, "ly_") {
		t.Fatalf("raw key %q missing ly_ prefix", raw)
	}
	if hash != HashKeyPeppered(raw) {
		t.Fatal("GenerateKey stored hash is not the peppered hash of the raw key")
	}
	if hash == HashKey(raw) {
		t.Fatal("GenerateKey stored a plain SHA-256 hash despite an installed pepper")
	}
}
