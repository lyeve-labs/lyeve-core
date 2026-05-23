package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type fakeEngine struct {
	get  map[string]string
	list []string
	err  error
}

func (f fakeEngine) Apply(context.Context, string, json.RawMessage) error { return nil }
func (f fakeEngine) Delete(context.Context, string) error                 { return nil }
func (f fakeEngine) PreviewDDL(context.Context, string, json.RawMessage) ([]DDLStatement, error) {
	return nil, nil
}
func (f fakeEngine) ApplyPending(context.Context) (int, error) { return 0, nil }
func (f fakeEngine) List(context.Context) ([]json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]json.RawMessage, 0, len(f.list))
	for _, raw := range f.list {
		out = append(out, json.RawMessage(raw))
	}
	return out, nil
}
func (f fakeEngine) Get(_ context.Context, name string) (json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	raw, ok := f.get[name]
	if !ok {
		return nil, ErrNotFound
	}
	return json.RawMessage(raw), nil
}
func (f fakeEngine) ValidateContent(context.Context, string, map[string]any) ([]SchemaValidationError, error) {
	return nil, nil
}

// nativeEngine answers definitions without going through JSON, which is what
// an engine holding them in memory does.
type nativeEngine struct {
	fakeEngine
	src SchemaSource
}

func (n nativeEngine) SchemaSource() SchemaSource { return n.src }

type fixedSource struct{ schemas []*Schema }

func (f fixedSource) GetByName(_ context.Context, name string) (*Schema, error) {
	for _, s := range f.schemas {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, ErrNotFound
}
func (f fixedSource) List(context.Context) ([]*Schema, error) { return f.schemas, nil }

// A reader built before the plugin starts has to answer from whatever
// registered later. This is the whole reason the content store does not hold a
// source directly.
func TestLazySchemaSource_FollowsWhatRegistersLater(t *testing.T) {
	var current SchemaSource
	src := LazySchemaSource(func() SchemaSource { return current })
	ctx := context.Background()

	if _, err := src.GetByName(ctx, "articles"); !errors.Is(err, ErrNoSchemaEngine) {
		t.Fatalf("before registration: err = %v, want ErrNoSchemaEngine", err)
	}

	current = fixedSource{schemas: []*Schema{{Name: "articles"}}}
	got, err := src.GetByName(ctx, "articles")
	if err != nil {
		t.Fatalf("after registration: %v", err)
	}
	if got.Name != "articles" {
		t.Fatalf("name = %q, want articles", got.Name)
	}

	current = nil
	if _, err := src.List(ctx); !errors.Is(err, ErrNoSchemaEngine) {
		t.Fatalf("after deregistration: err = %v, want ErrNoSchemaEngine", err)
	}
}

// A nil resolve is a source nobody wired. It reads as no engine rather than
// panicking, because the alternative is a boot that dies on the first content
// read instead of reporting itself degraded.
func TestLazySchemaSource_NilResolveIsNoEngine(t *testing.T) {
	src := LazySchemaSource(nil)
	if _, err := src.List(context.Background()); !errors.Is(err, ErrNoSchemaEngine) {
		t.Fatalf("err = %v, want ErrNoSchemaEngine", err)
	}
}

// An engine written without the content path in mind still feeds it, by
// decoding what its own read methods answer.
func TestSchemaSourceOf_DecodesAnEngineThatOffersNoReader(t *testing.T) {
	eng := fakeEngine{
		get:  map[string]string{"articles": `{"name":"articles","fields":[{"name":"title","field_type":"text"}]}`},
		list: []string{`{"name":"articles"}`, `{"name":"pages"}`},
	}
	src := SchemaSourceOf(eng)
	ctx := context.Background()

	got, err := src.GetByName(ctx, "articles")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Name != "articles" || len(got.Fields) != 1 || got.Fields[0].Name != "title" {
		t.Fatalf("decoded %+v, want articles with one field named title", got)
	}

	all, err := src.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 || all[0].Name != "articles" || all[1].Name != "pages" {
		t.Fatalf("List decoded %d schemas, want articles and pages", len(all))
	}
}

// The not-found an engine reports has to survive the decode, because the
// content path tells a missing content type from a missing engine by it.
func TestSchemaSourceOf_KeepsNotFound(t *testing.T) {
	src := SchemaSourceOf(fakeEngine{get: map[string]string{}})
	if _, err := src.GetByName(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// An engine that holds its definitions answers them directly. Decoding a
// document on every content read is the fallback, not the path.
func TestSchemaSourceOf_PrefersTheEnginesOwnReader(t *testing.T) {
	native := nativeEngine{
		fakeEngine: fakeEngine{err: errors.New("Get must not be reached")},
		src:        fixedSource{schemas: []*Schema{{Name: "articles"}}},
	}
	got, err := SchemaSourceOf(native).GetByName(context.Background(), "articles")
	if err != nil {
		t.Fatalf("GetByName went through the engine's JSON path: %v", err)
	}
	if got.Name != "articles" {
		t.Fatalf("name = %q, want articles", got.Name)
	}
}

// A nil engine is a registration that has not happened. Every read says so.
func TestSchemaSourceOf_NilEngineIsNoEngine(t *testing.T) {
	if _, err := SchemaSourceOf(nil).List(context.Background()); !errors.Is(err, ErrNoSchemaEngine) {
		t.Fatalf("err = %v, want ErrNoSchemaEngine", err)
	}
}

// A deferred source that resolves to nothing has to read as no engine. Asking
// it with a type assertion answers "present", which is how a degraded install
// would come to serve an empty world quietly.
func TestNoSchemaEngine_SeesThroughTheDeferral(t *testing.T) {
	var current SchemaSource
	src := LazySchemaSource(func() SchemaSource { return current })

	if !NoSchemaEngine(src) {
		t.Fatal("a deferred source resolving to nothing must read as no engine")
	}
	if _, absent := src.(AbsentSchemaSource); absent {
		t.Fatal("the deferred source is not itself the absent one, which is the trap this guards")
	}

	current = fixedSource{schemas: []*Schema{{Name: "articles"}}}
	if NoSchemaEngine(src) {
		t.Fatal("a registered source must read as present")
	}

	current = AbsentSchemaSource{}
	if !NoSchemaEngine(src) {
		t.Fatal("a source that resolves to the absent one must read as no engine")
	}
}

func TestNoSchemaEngine_NilIsNoEngine(t *testing.T) {
	if !NoSchemaEngine(nil) {
		t.Fatal("no source at all is no engine")
	}
}
