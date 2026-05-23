package core

import "testing"

// TestRegisterPlugin_BuffersUntilRegistrarIsWired pins registration across
// init order.
//
// A plugin repo imports pkg/core and calls RegisterPlugin from its own init.
// It does not import pkg/plugin, so no import edge orders the two init
// functions, and which one runs first is a property of the whole binary's
// import graph. One unrelated import elsewhere in the tree can flip it, so a
// registration that arrives first is buffered rather than sent to a nil func
// var, which would kill the binary at boot before any log line.
func TestRegisterPlugin_BuffersUntilRegistrarIsWired(t *testing.T) {
	registrarMu.Lock()
	savedReg, savedCaps, savedPending := registerFn, registerCapsFn, pendingPlugins
	registerFn, registerCapsFn, pendingPlugins = nil, nil, nil
	registrarMu.Unlock()
	t.Cleanup(func() {
		registrarMu.Lock()
		registerFn, registerCapsFn, pendingPlugins = savedReg, savedCaps, savedPending
		registrarMu.Unlock()
	})

	factory := func() Plugin { return nil }

	// Arrives before the registry exists. Must not panic, must not be lost.
	RegisterPlugin("early", factory)
	RegisterPluginWithCaps("early-caps", factory, CapDBRead)

	var plain, withCaps []string
	SetPluginRegistrar(
		func(name string, _ PluginFactory) { plain = append(plain, name) },
		func(name string, _ PluginFactory, _ Capability) { withCaps = append(withCaps, name) },
	)

	if len(plain) != 1 || plain[0] != "early" {
		t.Errorf("buffered registration not replayed: got %v", plain)
	}
	if len(withCaps) != 1 || withCaps[0] != "early-caps" {
		t.Errorf("buffered capability registration not replayed: got %v", withCaps)
	}

	// After wiring, registration goes straight through rather than buffering
	// a second time.
	RegisterPlugin("late", factory)
	if len(plain) != 2 || plain[1] != "late" {
		t.Errorf("registration after wiring did not reach the registry: got %v", plain)
	}
}
