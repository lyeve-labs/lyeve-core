package db_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// testRegistry is a schema engine and the reader over it, for the content
// store's tests.
//
// The content store reads definitions through core.SchemaSource, and this
// module builds none: the engine that creates content tables is supplied by a
// plugin. The supplied engine here proves that extension point, and it imports
// nothing but pkg/core, so a test that sets up through it exercises the same
// path an install does.
type testRegistry struct {
	engine *schemaengine.Engine
}

// The registry a pool already has, or a new one. The engine keeps its
// definitions in memory, so a second construction over one pool would start
// empty: a fixture would create a schema and the store built beside it would
// answer "not found" for the name the fixture had just applied.
var (
	registriesMu sync.Mutex
	registries   = map[db.DB]*testRegistry{}
)

func newTestRegistry(t *testing.T, pool db.DB) *testRegistry {
	t.Helper()
	registriesMu.Lock()
	defer registriesMu.Unlock()
	if r, ok := registries[pool]; ok {
		return r
	}
	r := &testRegistry{engine: schemaengine.New(testhost.New(pool))}
	registries[pool] = r
	return r
}

// Upsert records a definition and creates its table.
func (r *testRegistry) Upsert(ctx context.Context, sc *domain.Schema) error {
	def, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	return r.engine.Apply(ctx, sc.Name, def)
}

// Source is what the content store reads.
func (r *testRegistry) Source() core.SchemaSource { return r.engine.SchemaSource() }

// GetByName answers one definition, for a test that asserts on the shape it
// stored rather than on content.
func (r *testRegistry) GetByName(ctx context.Context, name string) (*core.Schema, error) {
	return r.engine.GetByName(ctx, name)
}

// Delete drops a definition and its table.
func (r *testRegistry) Delete(ctx context.Context, name string) error {
	return r.engine.Delete(ctx, name)
}
