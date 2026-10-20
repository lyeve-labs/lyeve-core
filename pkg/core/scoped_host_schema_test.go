package core

import (
	"context"
	"errors"
	"testing"
)

// A plugin without CapSchema gets an engine that refuses writes and reads
// through the one the host holds. No engine is held on a build without the
// plugin that supplies it, before that plugin starts and after it stops. The
// reads then answer ErrNoSchemaEngine, the way SchemaSource answers it. They
// used to forward to the missing engine and panic.
func TestScopedHostSchema_RefusesWhenNoEngineIsInstalled(t *testing.T) {
	ctx := context.Background()
	se := NewScopedHost(&stubHost{}, "reader", CapDBRead).Schema()
	if se == nil {
		t.Fatal("Schema() = nil, want the refusing engine a plugin without CapSchema gets")
	}

	if _, err := se.List(ctx); !errors.Is(err, ErrNoSchemaEngine) {
		t.Errorf("List = %v, want ErrNoSchemaEngine", err)
	}
	if _, err := se.Get(ctx, "posts"); !errors.Is(err, ErrNoSchemaEngine) {
		t.Errorf("Get = %v, want ErrNoSchemaEngine", err)
	}
	if _, err := se.PreviewDDL(ctx, "posts", nil); !errors.Is(err, ErrNoSchemaEngine) {
		t.Errorf("PreviewDDL = %v, want ErrNoSchemaEngine", err)
	}
	if _, err := se.ValidateContent(ctx, "posts", map[string]any{"title": "x"}); !errors.Is(err, ErrNoSchemaEngine) {
		t.Errorf("ValidateContent = %v, want ErrNoSchemaEngine", err)
	}
	// Several plugins read definitions through SchemaSourceOf, which wraps
	// whatever Schema() returned.
	if _, err := SchemaSourceOf(se).GetByName(ctx, "posts"); !errors.Is(err, ErrNoSchemaEngine) {
		t.Errorf("SchemaSourceOf(...).GetByName = %v, want ErrNoSchemaEngine", err)
	}

	if err := se.Apply(ctx, "posts", nil); !errors.Is(err, ErrCapDenied) {
		t.Errorf("Apply = %v, want ErrCapDenied", err)
	}
	if err := se.Delete(ctx, "posts"); !errors.Is(err, ErrCapDenied) {
		t.Errorf("Delete = %v, want ErrCapDenied", err)
	}
	if n, err := se.ApplyPending(ctx); !errors.Is(err, ErrCapDenied) || n != 0 {
		t.Errorf("ApplyPending = (%d, %v), want (0, ErrCapDenied)", n, err)
	}
}

// A plugin can keep the value from Start, and the engine belongs to another
// plugin, which can register after it and stop before it. The refusing engine
// therefore asks the host on every read: taken before an engine was
// registered, it reads through the engine once there is one, and answers
// ErrNoSchemaEngine again after the engine is withdrawn.
func TestScopedHostSchema_ReadsWhateverEngineTheHostHoldsNow(t *testing.T) {
	ctx := context.Background()
	inner := &schemaHost{}
	se := NewScopedHost(inner, "reader", CapDBRead).Schema()

	if _, err := se.List(ctx); !errors.Is(err, ErrNoSchemaEngine) {
		t.Fatalf("List before registration = %v, want ErrNoSchemaEngine", err)
	}
	inner.schema = &stubSchemaEngine{}
	list, err := se.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List after registration = (%d items, %v), want the engine's one", len(list), err)
	}
	inner.schema = nil
	if _, err := se.List(ctx); !errors.Is(err, ErrNoSchemaEngine) {
		t.Fatalf("List after withdrawal = %v, want ErrNoSchemaEngine", err)
	}
}

// A plugin holding CapSchema gets the host's engine itself, so with none
// installed it gets nil, which every caller of Schema() tests for.
func TestScopedHostSchema_GrantedPluginGetsTheHostsEngine(t *testing.T) {
	if se := NewScopedHost(&stubHost{}, "owner", CapSchema).Schema(); se != nil {
		t.Fatalf("Schema() with CapSchema and no engine = %T, want nil", se)
	}
	engine := &stubSchemaEngine{}
	if se := NewScopedHost(&schemaHost{schema: engine}, "owner", CapSchema).Schema(); se != engine {
		t.Fatalf("Schema() with CapSchema = %T, want the host's engine", se)
	}
}
