package schemaimport

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// fakeEngine stands in for the schema engine.
//
// What is under test here is the planning: which schemas an import would touch,
// in what order, and what it refuses to start. Whether the generated DDL is
// correct is the schema engine's own question, and the schema engine answers
// it against real databases.
//
// It refuses a definition whose relation points at a schema it does not hold,
// the way a database refuses a foreign key to a table that does not exist. An
// engine that accepted everything would let an import of two content types
// that reference each other pass here and fail on every real database.
type fakeEngine struct {
	stored  map[string]json.RawMessage
	applied []string
	defs    []domain.Schema
	failOn  string
}

func newFakeEngine(existing ...domain.Schema) *fakeEngine {
	e := &fakeEngine{stored: map[string]json.RawMessage{}}
	for _, s := range existing {
		raw, _ := json.Marshal(s)
		e.stored[s.Name] = raw
	}
	return e
}

func (e *fakeEngine) Apply(_ context.Context, name string, def json.RawMessage) error {
	if name == e.failOn {
		return errors.New("apply refused")
	}
	var sc domain.Schema
	if err := json.Unmarshal(def, &sc); err != nil {
		return err
	}
	for _, dep := range Dependencies(sc) {
		if _, ok := e.stored[dep]; !ok {
			return errors.New("relation " + name + " -> " + dep + ": referenced table does not exist")
		}
	}
	e.applied = append(e.applied, name)
	e.defs = append(e.defs, sc)
	e.stored[name] = def
	return nil
}

func (e *fakeEngine) PreviewDDL(_ context.Context, name string, _ json.RawMessage) ([]core.DDLStatement, error) {
	return []core.DDLStatement{{SQL: "CREATE TABLE _" + name + " (...)"}}, nil
}

func (e *fakeEngine) Get(_ context.Context, name string) (json.RawMessage, error) {
	raw, ok := e.stored[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return raw, nil
}

func (e *fakeEngine) List(context.Context) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(e.stored))
	for _, raw := range e.stored {
		out = append(out, raw)
	}
	return out, nil
}

func textSchema(name string) domain.Schema {
	return domain.Schema{
		Name:        name,
		DisplayName: displayName(name),
		Fields:      []domain.SchemaField{{Name: "title", FieldType: "text"}},
	}
}

func TestPlanImport_ClassifiesEachSchema(t *testing.T) {
	unchanged := textSchema("unchanged")
	changed := textSchema("changed")
	eng := newFakeEngine(unchanged, changed)

	modified := changed
	modified.Fields = append([]domain.SchemaField{}, changed.Fields...)
	modified.Fields = append(modified.Fields, domain.SchemaField{Name: "extra", FieldType: "number"})

	plan, err := PlanImport(context.Background(), eng, &Bundle{
		Schemas: []domain.Schema{unchanged, modified, textSchema("brand_new")},
	})
	require.NoError(t, err)

	actions := map[string]Action{}
	for _, s := range plan.Schemas {
		actions[s.Name] = s.Action
	}
	assert.Equal(t, ActionUnchanged, actions["unchanged"])
	assert.Equal(t, ActionUpdate, actions["changed"])
	assert.Equal(t, ActionCreate, actions["brand_new"])
	assert.Equal(t, 2, plan.Changes(), "an unchanged schema is not a change")
}

func TestPlanImport_UnchangedSchemaHasNoDDL(t *testing.T) {
	s := textSchema("article")
	plan, err := PlanImport(context.Background(), newFakeEngine(s), &Bundle{Schemas: []domain.Schema{s}})
	require.NoError(t, err)

	require.Len(t, plan.Schemas, 1)
	assert.Empty(t, plan.Schemas[0].DDL)
}

func TestPlanImport_ReportsMissingDependencies(t *testing.T) {
	// Applying this would fail part-way, leaving the target holding some of the
	// bundle and not the rest.
	plan, err := PlanImport(context.Background(), newFakeEngine(), &Bundle{
		Schemas: []domain.Schema{{
			Name:   "article",
			Fields: []domain.SchemaField{relationField("author", "person", domain.RelBelongsTo)},
		}},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"article"}, plan.Blocked())
	assert.Equal(t, []string{"person"}, plan.Schemas[0].Missing)
}

func TestPlanImport_DependencyAlreadyInTargetIsNotMissing(t *testing.T) {
	// Importing one part of a project at a time has to work.
	plan, err := PlanImport(context.Background(), newFakeEngine(textSchema("person")), &Bundle{
		Schemas: []domain.Schema{{
			Name:   "article",
			Fields: []domain.SchemaField{relationField("author", "person", domain.RelBelongsTo)},
		}},
	})
	require.NoError(t, err)
	assert.Empty(t, plan.Blocked())
}

func TestApplyBundle_AppliesInDependencyOrder(t *testing.T) {
	eng := newFakeEngine()
	_, err := ApplyBundle(context.Background(), eng, &Bundle{Schemas: []domain.Schema{
		{Name: "article", Fields: []domain.SchemaField{relationField("author", "person", domain.RelBelongsTo)}},
		textSchema("person"),
	}})
	require.NoError(t, err)

	assert.Equal(t, []string{"person", "article"}, eng.applied,
		"the referenced table has to exist before the foreign key is added")
}

func TestApplyBundle_RefusesToStartWhenBlocked(t *testing.T) {
	eng := newFakeEngine()
	_, err := ApplyBundle(context.Background(), eng, &Bundle{Schemas: []domain.Schema{
		textSchema("standalone"),
		{Name: "article", Fields: []domain.SchemaField{relationField("author", "person", domain.RelBelongsTo)}},
	}})
	require.Error(t, err)

	assert.Empty(t, eng.applied,
		"nothing is applied, rather than leaving the target holding half a project")
}

func TestApplyBundle_SkipsUnchangedSchemas(t *testing.T) {
	existing := textSchema("article")
	eng := newFakeEngine(existing)

	_, err := ApplyBundle(context.Background(), eng, &Bundle{
		Schemas: []domain.Schema{existing, textSchema("fresh")},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"fresh"}, eng.applied)
}

func TestApplyBundle_AppliesCyclicSchemasTwice(t *testing.T) {
	// Neither table can carry its constraint on the first pass, so each is
	// applied again once both exist.
	eng := newFakeEngine()
	plan, err := ApplyBundle(context.Background(), eng, &Bundle{Schemas: []domain.Schema{
		{Name: "a", Fields: []domain.SchemaField{relationField("b", "b", domain.RelBelongsTo)}},
		{Name: "b", Fields: []domain.SchemaField{relationField("a", "a", domain.RelBelongsTo)}},
	}})
	require.NoError(t, err)

	require.NotEmpty(t, plan.SecondPass)
	assert.Greater(t, len(eng.applied), 2, "the cycle members are applied a second time")
	for i, def := range eng.defs {
		first := i < 2
		assert.Equal(t, first, len(def.Fields) == 0,
			"apply %d of %q: the first pass leaves the relation out and the second carries it", i, def.Name)
	}
	for _, name := range []string{"a", "b"} {
		var stored domain.Schema
		require.NoError(t, json.Unmarshal(eng.stored[name], &stored))
		assert.Len(t, stored.Fields, 1, "%q ends with its relation", name)
	}
	for _, name := range plan.SecondPass {
		count := 0
		for _, applied := range eng.applied {
			if applied == name {
				count++
			}
		}
		assert.Equal(t, 2, count, "schema %q applied twice", name)
	}
}

func TestApplyBundle_ReportsTheSchemaThatFailed(t *testing.T) {
	eng := newFakeEngine()
	eng.failOn = "article"

	_, err := ApplyBundle(context.Background(), eng, &Bundle{
		Schemas: []domain.Schema{textSchema("article")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "article")
}

func TestPlanImport_RejectsANilEngine(t *testing.T) {
	_, err := PlanImport(context.Background(), nil, &Bundle{Schemas: []domain.Schema{textSchema("a")}})
	require.Error(t, err)
}

func TestExportBundle_RoundTripsThroughImport(t *testing.T) {
	source := newFakeEngine(
		domain.Schema{Name: "article", DisplayName: "Article", Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text", Required: true},
			relationField("author", "person", domain.RelBelongsTo),
		}},
		textSchema("person"),
	)

	bundle, err := ExportBundle(context.Background(), source, "test")
	require.NoError(t, err)
	require.Len(t, bundle.Schemas, 2)
	assert.Equal(t, "person", bundle.Schemas[0].Name, "exported in dependency order")

	// The exported file has to be readable by the importer, or an export is
	// just a backup nobody can restore.
	rendered, err := bundle.MarshalYAML()
	require.NoError(t, err)
	parsed, err := ParseBundle(rendered)
	require.NoError(t, err)

	target := newFakeEngine()
	_, err = ApplyBundle(context.Background(), target, parsed)
	require.NoError(t, err)
	assert.Equal(t, []string{"person", "article"}, target.applied)
}

func TestExportBundle_RejectsANilEngine(t *testing.T) {
	_, err := ExportBundle(context.Background(), nil, "test")
	require.Error(t, err)
}
