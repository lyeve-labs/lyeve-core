package auth

import (
	"testing"

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
