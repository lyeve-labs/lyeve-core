package api

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// fakeSchemaSource is an in-memory registry for the content handler's tests.
// A test that wants a content type supplies one through the interface the
// handler reads, which is what an install with a schema plugin does.
type fakeSchemaSource struct {
	schemas map[string]*core.Schema
	err     error
}

func newFakeSchemaSource(defs ...*core.Schema) *fakeSchemaSource {
	m := make(map[string]*core.Schema, len(defs))
	for _, d := range defs {
		m[d.Name] = d
	}
	return &fakeSchemaSource{schemas: m}
}

func (f *fakeSchemaSource) GetByName(_ context.Context, name string) (*core.Schema, error) {
	if f.err != nil {
		return nil, f.err
	}
	sc, ok := f.schemas[name]
	if !ok {
		return nil, core.ErrNotFound
	}
	return sc, nil
}

func (f *fakeSchemaSource) List(context.Context) ([]*core.Schema, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*core.Schema, 0, len(f.schemas))
	for _, sc := range f.schemas {
		out = append(out, sc)
	}
	return out, nil
}

var _ core.SchemaSource = (*fakeSchemaSource)(nil)
