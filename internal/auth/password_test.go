package auth

import (
	"sort"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The dummy hash exists so an unknown email costs the same bcrypt work as a
// known one. That only holds while its cost factor matches the one real
// hashes are written at: bcrypt doubles per factor, so a dummy one step
// higher makes every unknown email reliably twice as slow, which is the
// enumeration signal the dummy exists to remove.
//
// Deliberately checked here rather than by timing a request. The gap is exact
// and a wall clock cannot see it on a loaded runner.
func TestDummyBcryptHash_CostMatchesTheCostRealHashesUse(t *testing.T) {
	cost, err := bcrypt.Cost([]byte(DummyBcryptHash))
	if err != nil {
		t.Fatalf("DummyBcryptHash is not a bcrypt hash: %v", err)
	}
	if cost != bcrypt.DefaultCost {
		t.Errorf("dummy hash cost = %d, real hashes are written at %d; "+
			"an unknown email would take 2^%d times the work of a known one",
			cost, bcrypt.DefaultCost, cost-bcrypt.DefaultCost)
	}

	real, err := HashPassword("bcrypt", "any-password-at-all")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	realCost, err := bcrypt.Cost([]byte(real))
	if err != nil {
		t.Fatalf("cost of a real hash: %v", err)
	}
	if realCost != cost {
		t.Errorf("real hash cost = %d, dummy = %d", realCost, cost)
	}
}

// An account with no password stores an empty hash or a marker, and a sign-in
// attempt against it is refused. The refusal costs the bcrypt compare an
// unknown address gets, or a fast answer tells the caller the account exists.
func TestVerifyPassword_RefusedHashesCostOneBcrypt(t *testing.T) {
	medianOf := func(hash string) time.Duration {
		var took []time.Duration
		for i := 0; i < 7; i++ {
			start := time.Now()
			_ = VerifyPassword("bcrypt", hash, "not-the-password")
			took = append(took, time.Since(start))
		}
		sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
		return took[len(took)/2]
	}
	unknown := medianOf(DummyBcryptHash)
	for _, hash := range []string{"", OAuthOnlyMarker, ExternalIssuerMarker} {
		if err := VerifyPassword("bcrypt", hash, "not-the-password"); err == nil {
			t.Errorf("hash %q verified a password", hash)
		}
		if err := VerifyPassword("bcrypt", hash, ""); err == nil {
			t.Errorf("hash %q verified an empty password", hash)
		}
		if got := medianOf(hash); float64(got) < 0.5*float64(unknown) {
			t.Errorf("hash %q is refused in %v, an unknown address costs %v", hash, got, unknown)
		}
	}
}
