package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// engineMode reads LYEVE_MODE. Empty is the engine with a database. The only
// other value is core.EngineModeStateless. Anything else stops the boot, since
// a misspelled mode would otherwise boot the database engine the operator was
// trying not to run.
func engineMode() (string, error) {
	raw := strings.TrimSpace(lookup("LYEVE_MODE"))
	switch strings.ToLower(raw) {
	case "":
		return "", nil
	case core.EngineModeStateless:
		return core.EngineModeStateless, nil
	default:
		return "", fmt.Errorf("LYEVE_MODE must be %q or unset, got %q", core.EngineModeStateless, raw)
	}
}

// checkStatelessMode refuses the settings a stateless engine cannot honor.
// Each of them names something the operator expects to happen, and booting
// without it would look like it did: a DATABASE_URL an operator believes
// holds their data, a second tenant, a setup screen that would ask for a
// database the mode never connects to.
func checkStatelessMode() error {
	var conflicts []string
	for _, key := range []string{"DATABASE_URL", "DATABASE_REPLICA_URL"} {
		if lookup(key) != "" {
			conflicts = append(conflicts, key+" is set, but a stateless engine never connects to a database; unset it, or unset LYEVE_MODE")
		}
	}
	if on, err := strconv.ParseBool(lookup("LYEVE_SETUP_MODE")); err == nil && on {
		conflicts = append(conflicts, "LYEVE_SETUP_MODE asks for a database the stateless engine never connects to; unset one of them")
	}
	if on, err := strconv.ParseBool(lookup("MULTI_TENANT")); err == nil && on {
		conflicts = append(conflicts, "MULTI_TENANT needs the tenant registry, which lives in the database; a stateless engine serves one tenant")
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("LYEVE_MODE=stateless: %s", strings.Join(conflicts, "; "))
}

// Stateless reports whether the engine runs with no database.
func (c *Config) Stateless() bool {
	return c != nil && c.Mode == core.EngineModeStateless
}
