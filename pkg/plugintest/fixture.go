package plugintest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest/mockhost"
)

// Fixture types

// PluginFixture captures the observable contract of one plugin: hooks it
// emits, tables it covers, and routes it exposes, with no plugin logic.
type PluginFixture struct {
	PluginName string `json:"plugin_name"`
	Version    string `json:"version,omitempty"`

	// HooksEmitted lists every hook the plugin publishes, with the
	// event type, target schema, and the keys present in Event.Data.
	HooksEmitted []HookEmitted `json:"hooks_emitted,omitempty"`

	// TablesCovered lists table names the plugin has declared via
	// core.RegisterCoveredTable (TenantPurgeHandlers).
	TablesCovered []string `json:"tables_covered,omitempty"`

	// RoutesExposed lists every HTTP route the plugin declares via
	// Routes().
	RoutesExposed []RouteExposed `json:"routes_exposed,omitempty"`
}

// HookEmitted describes one hook that the plugin publishes.
type HookEmitted struct {
	EventType core.EventType `json:"event_type"`
	Schema    string         `json:"schema"`
	// PayloadKeys are the top-level keys present in Event.Data when
	// this hook is fired. Empty slice = no data, nil = unknown shape.
	PayloadKeys []string `json:"payload_keys,omitempty"`
}

// RouteExposed describes one HTTP route the plugin claims.
type RouteExposed struct {
	Method  string          `json:"method"`
	Pattern string          `json:"pattern"`
	Group   core.RouteGroup `json:"group"`
}

// Validate checks the fixture for structural correctness.
func (f *PluginFixture) Validate() error {
	if f.PluginName == "" {
		return fmt.Errorf("plugin_name is required")
	}
	for _, h := range f.HooksEmitted {
		if h.EventType == "" {
			return fmt.Errorf("hook_emitted event_type is required")
		}
	}
	for _, r := range f.RoutesExposed {
		if r.Method == "" || r.Pattern == "" {
			return fmt.Errorf("route_exposed method and pattern are required")
		}
	}
	return nil
}

// Loaded fixture: wraps MockHost with contract knowledge

// LoadedFixture is a PluginFixture loaded into a MockHost. It stands in for a
// plugin whose code the test does not have: firing events, listing tables,
// and enumerating routes.
type LoadedFixture struct {
	fixture *PluginFixture
	mock    *mockhost.MockHost
}

// LoadFixture loads a contract fixture into a MockHost. FireEvent publishes
// events through the mock's hook bus. Tables and Routes return the declared
// contract. Panics on nil or invalid fixture.
func LoadFixture(mock *mockhost.MockHost, fixture *PluginFixture) *LoadedFixture {
	if fixture == nil {
		panic("plugintest: nil fixture passed to LoadFixture")
	}
	if err := fixture.Validate(); err != nil {
		panic("plugintest: invalid fixture: " + err.Error())
	}
	return &LoadedFixture{
		fixture: fixture,
		mock:    mock,
	}
}

// Fixture returns the underlying PluginFixture.
func (lf *LoadedFixture) Fixture() *PluginFixture { return lf.fixture }

// Mock returns the MockHost this fixture was loaded into.
func (lf *LoadedFixture) Mock() *mockhost.MockHost { return lf.mock }

// Tables returns a copy of the covered table names.
func (lf *LoadedFixture) Tables() []string {
	out := make([]string, len(lf.fixture.TablesCovered))
	copy(out, lf.fixture.TablesCovered)
	return out
}

// Routes returns a copy of the exposed routes.
func (lf *LoadedFixture) Routes() []RouteExposed {
	out := make([]RouteExposed, len(lf.fixture.RoutesExposed))
	copy(out, lf.fixture.RoutesExposed)
	return out
}

// Hooks returns a copy of the emitted hook declarations.
func (lf *LoadedFixture) Hooks() []HookEmitted {
	out := make([]HookEmitted, len(lf.fixture.HooksEmitted))
	copy(out, lf.fixture.HooksEmitted)
	return out
}

// Event simulation

// FireEvent publishes an event through the mock host's hook bus, standing in
// for another plugin firing a lifecycle hook. Returns an error when the
// (eventType, schema) pair is not declared in the fixture's contract.
// The payload is placed into Event.Data.
func (lf *LoadedFixture) FireEvent(ctx context.Context, eventType core.EventType, schema string, payload map[string]any) error {
	// Validate against contract
	if !lf.fixture.hasHook(eventType, schema) {
		return fmt.Errorf("plugintest: fixture %q does not declare hook %s on schema %q",
			lf.fixture.PluginName, eventType, schema)
	}

	event := core.Event{
		Type:   eventType,
		Schema: schema,
		Data:   payload,
		Source: core.SourcePlugin,
	}
	var pub core.HookPublisher = lf.mock.Hooks()
	return pub.Publish(ctx, event)
}

// FireAllEvents publishes every hook declared in the fixture in order with
// nil payloads. Use for smoke-testing that a consuming plugin handles all
// events without crashing.
func (lf *LoadedFixture) FireAllEvents(ctx context.Context) error {
	for _, h := range lf.fixture.HooksEmitted {
		if err := lf.FireEvent(ctx, h.EventType, h.Schema, nil); err != nil {
			return err
		}
	}
	return nil
}

// HasHook reports whether the fixture declares the given hook.
func (lf *LoadedFixture) HasHook(eventType core.EventType, schema string) bool {
	return lf.fixture.hasHook(eventType, schema)
}

func (f *PluginFixture) hasHook(eventType core.EventType, schema string) bool {
	for _, h := range f.HooksEmitted {
		if h.EventType == eventType && h.Schema == schema {
			return true
		}
	}
	return false
}

// Serialization helpers

// ParseFixtureJSON parses a PluginFixture from raw JSON and validates it.
func ParseFixtureJSON(data []byte) (*PluginFixture, error) {
	var f PluginFixture
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("plugintest: parse fixture: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("plugintest: invalid fixture: %w", err)
	}
	return &f, nil
}

// LoadFixtureJSON parses JSON and loads it into a MockHost in one call.
func LoadFixtureJSON(mock *mockhost.MockHost, data []byte) (*LoadedFixture, error) {
	f, err := ParseFixtureJSON(data)
	if err != nil {
		return nil, err
	}
	return LoadFixture(mock, f), nil
}
