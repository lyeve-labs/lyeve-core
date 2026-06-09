package plugintest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest/mockhost"
)

func TestPluginFixture_Validate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		f := &PluginFixture{PluginName: "example"}
		if err := f.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})

	t.Run("missing_plugin_name", func(t *testing.T) {
		f := &PluginFixture{}
		if err := f.Validate(); err == nil {
			t.Fatal("expected error for missing plugin_name")
		}
	})

	t.Run("missing_hook_event_type", func(t *testing.T) {
		f := &PluginFixture{
			PluginName:   "example",
			HooksEmitted: []HookEmitted{{Schema: "content"}},
		}
		if err := f.Validate(); err == nil {
			t.Fatal("expected error for missing hook event_type")
		}
	})

	t.Run("missing_route_method", func(t *testing.T) {
		f := &PluginFixture{
			PluginName:    "example",
			RoutesExposed: []RouteExposed{{Pattern: "/api/example"}},
		}
		if err := f.Validate(); err == nil {
			t.Fatal("expected error for missing route method")
		}
	})
}

func TestParseFixtureJSON(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		data := []byte(`{
			"plugin_name": "example",
			"hooks_emitted": [
				{"event_type": "after_create", "schema": "content", "payload_keys": ["id", "title"]}
			],
			"tables_covered": ["sys_example_items"],
			"routes_exposed": [
				{"method": "GET", "pattern": "/api/admin/example", "group": "admin"}
			]
		}`)
		f, err := ParseFixtureJSON(data)
		if err != nil {
			t.Fatalf("ParseFixtureJSON: %v", err)
		}
		if f.PluginName != "example" {
			t.Errorf("expected plugin_name 'example', got %q", f.PluginName)
		}
		if len(f.HooksEmitted) != 1 {
			t.Fatalf("expected 1 hook, got %d", len(f.HooksEmitted))
		}
		if f.HooksEmitted[0].EventType != core.AfterCreate {
			t.Errorf("expected after_create, got %v", f.HooksEmitted[0].EventType)
		}
		if len(f.TablesCovered) != 1 || f.TablesCovered[0] != "sys_example_items" {
			t.Errorf("unexpected tables: %v", f.TablesCovered)
		}
		if len(f.RoutesExposed) != 1 {
			t.Fatalf("expected 1 route, got %d", len(f.RoutesExposed))
		}
		if f.RoutesExposed[0].Group != core.GroupAdmin {
			t.Errorf("expected admin group, got %v", f.RoutesExposed[0].Group)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		_, err := ParseFixtureJSON([]byte(`{bad`))
		if err == nil {
			t.Fatal("expected parse error")
		}
	})

	t.Run("missing_plugin_name", func(t *testing.T) {
		_, err := ParseFixtureJSON([]byte(`{}`))
		if err == nil {
			t.Fatal("expected validation error")
		}
	})
}

func TestLoadFixture(t *testing.T) {
	mock := mockhost.New(t)
	fixture := &PluginFixture{
		PluginName: "example",
		HooksEmitted: []HookEmitted{
			{EventType: core.AfterCreate, Schema: "content", PayloadKeys: []string{"id", "title"}},
			{EventType: core.AfterUpdate, Schema: "content", PayloadKeys: []string{"id", "title"}},
		},
		TablesCovered: []string{"sys_example_items", "sys_example_notes"},
		RoutesExposed: []RouteExposed{
			{Method: "GET", Pattern: "/api/admin/example", Group: core.GroupAdmin},
		},
	}

	lf := LoadFixture(mock, fixture)

	if lf.Fixture() != fixture {
		t.Error("Fixture() should return the same fixture")
	}
	if lf.Mock() != mock {
		t.Error("Mock() should return the same mock")
	}
	if len(lf.Tables()) != 2 {
		t.Errorf("expected 2 tables, got %d", len(lf.Tables()))
	}
	if len(lf.Routes()) != 1 {
		t.Errorf("expected 1 route, got %d", len(lf.Routes()))
	}
	if len(lf.Hooks()) != 2 {
		t.Errorf("expected 2 hooks, got %d", len(lf.Hooks()))
	}
}

func TestLoadedFixture_FireEvent(t *testing.T) {
	ctx := context.Background()
	mock := mockhost.New(t)
	fixture := &PluginFixture{
		PluginName: "example",
		HooksEmitted: []HookEmitted{
			{EventType: core.AfterCreate, Schema: "content", PayloadKeys: []string{"id", "title"}},
		},
	}

	lf := LoadFixture(mock, fixture)

	// Fire a declared hook: should succeed
	err := lf.FireEvent(ctx, core.AfterCreate, "content", map[string]any{
		"id":    "abc",
		"title": "Hello",
	})
	if err != nil {
		t.Fatalf("FireEvent declared hook: %v", err)
	}

	// Verify the event was published through the mock's hook spy.
	// mock.Hooks() returns *hookSpy which has AssertPublished.
	mock.Hooks().AssertPublished(t, core.AfterCreate, "content")

	// Fire an undeclared hook: should fail
	err = lf.FireEvent(ctx, core.BeforeDelete, "content", nil)
	if err == nil {
		t.Fatal("expected error for undeclared hook")
	}
}

func TestLoadedFixture_FireAllEvents(t *testing.T) {
	ctx := context.Background()
	mock := mockhost.New(t)
	fixture := &PluginFixture{
		PluginName: "example",
		HooksEmitted: []HookEmitted{
			{EventType: core.AfterCreate, Schema: "content"},
			{EventType: core.AfterUpdate, Schema: "content"},
			{EventType: core.AfterDelete, Schema: "content"},
		},
	}

	lf := LoadFixture(mock, fixture)
	err := lf.FireAllEvents(ctx)
	if err != nil {
		t.Fatalf("FireAllEvents: %v", err)
	}
}

func TestLoadedFixture_HasHook(t *testing.T) {
	mock := mockhost.New(t)
	fixture := &PluginFixture{
		PluginName: "example",
		HooksEmitted: []HookEmitted{
			{EventType: core.AfterCreate, Schema: "content"},
		},
	}

	lf := LoadFixture(mock, fixture)

	if !lf.HasHook(core.AfterCreate, "content") {
		t.Error("expected HasHook true for declared hook")
	}
	if lf.HasHook(core.BeforeDelete, "content") {
		t.Error("expected HasHook false for undeclared hook")
	}
	if lf.HasHook(core.AfterCreate, "different_schema") {
		t.Error("expected HasHook false for different schema")
	}
}

func TestLoadedFixture_SubscribeAndReact(t *testing.T) {
	// This is the core scenario: a custom plugin subscribes to a hook that
	// another plugin emits (per the contract fixture). LoadFixture stands in
	// for a plugin whose code the test does not have.
	ctx := context.Background()
	mock := mockhost.New(t)
	fixture := &PluginFixture{
		PluginName: "example",
		HooksEmitted: []HookEmitted{
			{EventType: core.AfterCreate, Schema: "content", PayloadKeys: []string{"id", "title"}},
		},
	}

	lf := LoadFixture(mock, fixture)

	// Custom plugin subscribes to the hook the other plugin emits
	reactionReceived := false
	mock.Hooks().Subscribe("content", core.AfterCreate, func(ctx context.Context, e core.Event) error {
		reactionReceived = true
		if e.Data["id"] != "abc" {
			t.Errorf("expected id='abc', got %v", e.Data["id"])
		}
		if e.Data["title"] != "Hello" {
			t.Errorf("expected title='Hello', got %v", e.Data["title"])
		}
		return nil
	})

	// Stand in for the other plugin firing its hook
	err := lf.FireEvent(ctx, core.AfterCreate, "content", map[string]any{
		"id":    "abc",
		"title": "Hello",
	})
	if err != nil {
		t.Fatalf("FireEvent: %v", err)
	}

	if !reactionReceived {
		t.Error("expected custom plugin reaction to be triggered")
	}
}

func TestLoadFixtureJSON(t *testing.T) {
	mock := mockhost.New(t)
	data := []byte(`{
		"plugin_name": "example",
		"hooks_emitted": [
			{"event_type": "after_create", "schema": "content", "payload_keys": ["id"]}
		],
		"tables_covered": ["sys_example_notes"],
		"routes_exposed": [
			{"method": "POST", "pattern": "/api/admin/example", "group": "admin"}
		]
	}`)

	lf, err := LoadFixtureJSON(mock, data)
	if err != nil {
		t.Fatalf("LoadFixtureJSON: %v", err)
	}
	if lf.Fixture().PluginName != "example" {
		t.Errorf("expected example, got %s", lf.Fixture().PluginName)
	}
}

func TestLoadFixture_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil fixture")
		}
	}()
	mock := mockhost.New(t)
	LoadFixture(mock, nil)
}

func TestLoadFixture_PanicsOnInvalid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on invalid fixture")
		}
	}()
	mock := mockhost.New(t)
	LoadFixture(mock, &PluginFixture{}) // missing PluginName
}

// Round-trip: marshal -> unmarshal -> validate -> load
func TestFixtureJSON_RoundTrip(t *testing.T) {
	original := &PluginFixture{
		PluginName: "example",
		Version:    "1.0.0",
		HooksEmitted: []HookEmitted{
			{EventType: core.AfterCreate, Schema: "content", PayloadKeys: []string{"id", "title"}},
			{EventType: core.BeforeDelete, Schema: "content"},
		},
		TablesCovered: []string{"sys_example_items", "sys_example_notes"},
		RoutesExposed: []RouteExposed{
			{Method: "GET", Pattern: "/api/admin/example", Group: core.GroupAdmin},
			{Method: "POST", Pattern: "/api/admin/example/generate", Group: core.GroupAdmin},
		},
	}

	// Marshal
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Unmarshal
	parsed, err := ParseFixtureJSON(data)
	if err != nil {
		t.Fatalf("ParseFixtureJSON: %v", err)
	}

	if parsed.PluginName != original.PluginName {
		t.Errorf("plugin_name: got %q, want %q", parsed.PluginName, original.PluginName)
	}
	if parsed.Version != original.Version {
		t.Errorf("version: got %q, want %q", parsed.Version, original.Version)
	}
	if len(parsed.HooksEmitted) != len(original.HooksEmitted) {
		t.Fatalf("hooks: got %d, want %d", len(parsed.HooksEmitted), len(original.HooksEmitted))
	}
	for i, h := range parsed.HooksEmitted {
		if h.EventType != original.HooksEmitted[i].EventType {
			t.Errorf("hook[%d].event_type: got %v, want %v", i, h.EventType, original.HooksEmitted[i].EventType)
		}
		if h.Schema != original.HooksEmitted[i].Schema {
			t.Errorf("hook[%d].schema: got %q, want %q", i, h.Schema, original.HooksEmitted[i].Schema)
		}
		if len(h.PayloadKeys) != len(original.HooksEmitted[i].PayloadKeys) {
			t.Errorf("hook[%d].payload_keys: got %d, want %d", i, len(h.PayloadKeys), len(original.HooksEmitted[i].PayloadKeys))
		}
	}
	if len(parsed.TablesCovered) != len(original.TablesCovered) {
		t.Fatalf("tables: got %d, want %d", len(parsed.TablesCovered), len(original.TablesCovered))
	}
	if len(parsed.RoutesExposed) != len(original.RoutesExposed) {
		t.Fatalf("routes: got %d, want %d", len(parsed.RoutesExposed), len(original.RoutesExposed))
	}
}

func TestFixtureMarshal_OmitsEmpty(t *testing.T) {
	// Verify that omitempty works correctly for HooksEmitted and TablesCovered
	f := &PluginFixture{PluginName: "minimal"}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["hooks_emitted"]; ok {
		t.Error("expected hooks_emitted to be omitted when empty")
	}
	if _, ok := m["tables_covered"]; ok {
		t.Error("expected tables_covered to be omitted when empty")
	}
	if _, ok := m["routes_exposed"]; ok {
		t.Error("expected routes_exposed to be omitted when empty")
	}
}
