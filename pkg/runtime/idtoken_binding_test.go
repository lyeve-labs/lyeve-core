package runtime_test

import (
	"testing"

	_ "github.com/lyeve-labs/lyeve-core/pkg/runtime"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// The validator is bound by internal/auth's init(). This asserts the boot path
// actually links that package, which is the difference between a validator
// that exists and one the shipped binary can reach.
func TestIDTokenValidatorIsBoundByTheBootPath(t *testing.T) {
	if security.ValidateIDToken == nil {
		t.Fatal("security.ValidateIDToken is nil after importing pkg/runtime: OIDC login will refuse every login")
	}
}
