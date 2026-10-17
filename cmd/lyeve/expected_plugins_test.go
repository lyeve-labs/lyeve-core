package main

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The boot check only proves what expectedPlugins names, so a plugin this
// build imports but the list omits could fail to register and the binary
// would still start. The registry is read from the imports themselves, which
// makes the next plugin added without a list entry fail here as well.
func TestExpectedPlugins_MatchesRegisteredPlugins(t *testing.T) {
	registered := core.RegisteredPlugins()
	if len(registered) == 0 {
		t.Fatal("no plugin registered, so the comparison would prove nothing")
	}
	if unlisted := missingPlugins(registered, expectedPlugins); len(unlisted) > 0 {
		t.Errorf("registered but absent from expectedPlugins: %v", unlisted)
	}
	if unregistered := missingPlugins(expectedPlugins, registered); len(unregistered) > 0 {
		t.Errorf("listed in expectedPlugins but never registered: %v", unregistered)
	}
}
