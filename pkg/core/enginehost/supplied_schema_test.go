package enginehost

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The published kernel is only a kernel if someone outside this module can
// supply the schema engine. That means the host has to accept one through the
// public interface and hand it back through Schema(), with no internal type
// anywhere in the path.
func TestEngineHost_AcceptsASuppliedSchemaEngine(t *testing.T) {
	h := &engineHost{}

	reg, ok := any(h).(core.SchemaEngineRegistrar)
	if !ok {
		t.Fatal("the engine host does not accept a supplied schema engine: no SchemaEngineRegistrar")
	}

	supplied := schemaengine.New(nil)
	reg.RegisterSchemaEngine(supplied)

	got := h.Schema()
	if got == nil {
		t.Fatal("Schema() is nil after an engine was registered")
	}
	if got != core.SchemaEngine(supplied) {
		t.Fatalf("Schema() returned %T, want the registered engine", got)
	}
}

// A plugin unregisters when it stops, so the host has to forget the engine
// rather than serve a stopped plugin's implementation.
func TestEngineHost_ForgetsTheEngineWhenThePluginStops(t *testing.T) {
	h := &engineHost{}
	reg, ok := any(h).(core.SchemaEngineRegistrar)
	if !ok {
		t.Fatal("the engine host does not accept a supplied schema engine: no SchemaEngineRegistrar")
	}

	reg.RegisterSchemaEngine(schemaengine.New(nil))
	reg.RegisterSchemaEngine(nil)

	if got := h.Schema(); got != nil {
		t.Fatalf("Schema() = %T after deregistration, want nil", got)
	}
}
