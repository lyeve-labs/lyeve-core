package schema

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// The system fields the engine injects into every definition, which the
// content table's shape depends on.

func TestInjectIDField_EmptyFields(t *testing.T) {
	t.Parallel()
	got := InjectIDField(nil)
	if len(got) != 1 || got[0].Name != "id" || got[0].FieldType != "uid" || !got[0].Required || !got[0].System {
		t.Errorf("expected single id field, got %+v", got)
	}
}

func TestInjectIDField_Idempotent(t *testing.T) {
	t.Parallel()
	fields := []domain.SchemaField{
		{Name: "title", FieldType: "text"},
	}
	once := InjectIDField(fields)
	twice := InjectIDField(once)
	if len(once) != len(twice) {
		t.Errorf("injectIDField not idempotent: once=%d twice=%d", len(once), len(twice))
	}
	if twice[0].Name != "id" {
		t.Errorf("id not first after second injection: %+v", twice)
	}
}

func TestInjectIDField_PushesExistingToFront(t *testing.T) {
	t.Parallel()
	fields := []domain.SchemaField{
		{Name: "title", FieldType: "text"},
		{Name: "id", FieldType: "text"},
	}
	got := InjectIDField(fields)
	if len(got) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(got))
	}
	if got[0].Name != "id" || got[0].FieldType != "uid" {
		t.Errorf("id field wrong: %+v", got[0])
	}
	if got[1].Name != "title" {
		t.Errorf("second field should be title, got %s", got[1].Name)
	}
}

// injectFKFields: pure function tests

func TestInjectFKFields_NoRelations(t *testing.T) {
	t.Parallel()
	fields := []domain.SchemaField{
		{Name: "title", FieldType: "text"},
		{Name: "body", FieldType: "text"},
	}
	got := InjectFKFields(fields)
	if len(got) != 2 {
		t.Errorf("expected unchanged, got %d fields", len(got))
	}
}

func TestInjectFKFields_BelongsToRelation(t *testing.T) {
	t.Parallel()
	fields := []domain.SchemaField{
		{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo, Required: true},
		{Name: "title", FieldType: "text"},
	}
	got := InjectFKFields(fields)
	if len(got) != 3 {
		t.Fatalf("expected 3 fields, got %d: %+v", len(got), got)
	}
	if got[0].Name != "author" {
		t.Errorf("first field should be author, got %s", got[0].Name)
	}
	if got[1].Name != "author_id" || !got[1].System || got[1].FieldType != "uid" {
		t.Errorf("fk field wrong: %+v", got[1])
	}
	if got[2].Name != "title" {
		t.Errorf("last field should be title, got %s", got[2].Name)
	}
}

func TestInjectFKFields_ManyToManySkip(t *testing.T) {
	t.Parallel()
	fields := []domain.SchemaField{
		{Name: "tags", FieldType: "relation", RelationType: domain.RelManyToMany},
	}
	got := InjectFKFields(fields)
	if len(got) != 1 {
		t.Errorf("many_to_many should not generate FK, got %d fields", len(got))
	}
}

func TestInjectFKFields_Idempotent(t *testing.T) {
	t.Parallel()
	fields := []domain.SchemaField{
		{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo},
	}
	once := InjectFKFields(fields)
	twice := InjectFKFields(once)
	if len(once) != len(twice) {
		t.Errorf("injectFKFields not idempotent: once=%d twice=%d", len(once), len(twice))
	}
}

func TestInjectFKFields_CustomFKName(t *testing.T) {
	t.Parallel()
	fields := []domain.SchemaField{
		{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationFKName: "writer_id"},
	}
	got := InjectFKFields(fields)
	if len(got) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(got))
	}
	if got[1].Name != "writer_id" {
		t.Errorf("expected custom FK name writer_id, got %s", got[1].Name)
	}
}

func TestInjectFKFields_MultipleRelations(t *testing.T) {
	t.Parallel()
	fields := []domain.SchemaField{
		{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo},
		{Name: "category", FieldType: "relation", RelationType: domain.RelBelongsTo},
		{Name: "title", FieldType: "text"},
	}
	got := InjectFKFields(fields)
	if len(got) != 5 {
		t.Fatalf("expected 5 fields, got %d: %+v", len(got), got)
	}
	if got[1].Name != "author_id" {
		t.Errorf("expected author_id after author, got %s", got[1].Name)
	}
	if got[3].Name != "category_id" {
		t.Errorf("expected category_id after category, got %s", got[3].Name)
	}
}

// injectTimestampFields

func TestInjectTimestampFields_NoFlags(t *testing.T) {
	t.Parallel()
	sc := domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
		},
	}
	InjectTimestampFields(&sc)
	if len(sc.Fields) != 1 {
		t.Errorf("no timestamps expected, got %d fields", len(sc.Fields))
	}
}

func TestInjectTimestampFields_WithCreatedAt(t *testing.T) {
	t.Parallel()
	sc := domain.Schema{
		WithCreatedAt: true,
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
		},
	}
	InjectTimestampFields(&sc)
	if len(sc.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(sc.Fields))
	}
	if sc.Fields[1].Name != "created_at" || !sc.Fields[1].System {
		t.Errorf("created_at field wrong: %+v", sc.Fields[1])
	}
}

func TestInjectTimestampFields_WithUpdatedAt(t *testing.T) {
	t.Parallel()
	sc := domain.Schema{
		WithUpdatedAt: true,
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
		},
	}
	InjectTimestampFields(&sc)
	if len(sc.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(sc.Fields))
	}
	if sc.Fields[1].Name != "updated_at" || !sc.Fields[1].System {
		t.Errorf("updated_at field wrong: %+v", sc.Fields[1])
	}
}

func TestInjectTimestampFields_Both(t *testing.T) {
	t.Parallel()
	sc := domain.Schema{
		WithCreatedAt: true,
		WithUpdatedAt: true,
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
		},
	}
	InjectTimestampFields(&sc)
	if len(sc.Fields) != 3 {
		t.Fatalf("expected 3 fields, got %d: %+v", len(sc.Fields), sc.Fields)
	}
	if sc.Fields[1].Name != "created_at" {
		t.Errorf("second field should be created_at, got %s", sc.Fields[1].Name)
	}
	if sc.Fields[2].Name != "updated_at" {
		t.Errorf("third field should be updated_at, got %s", sc.Fields[2].Name)
	}
}

func TestInjectTimestampFields_StripsUserSupplied(t *testing.T) {
	t.Parallel()
	sc := domain.Schema{
		WithCreatedAt: true,
		Fields: []domain.SchemaField{
			{Name: "created_at", FieldType: "text"},
			{Name: "title", FieldType: "text"},
		},
	}
	InjectTimestampFields(&sc)
	if len(sc.Fields) != 2 {
		t.Fatalf("expected 2 fields after stripping, got %d: %+v", len(sc.Fields), sc.Fields)
	}
	if sc.Fields[0].Name != "title" {
		t.Errorf("first should be title, got %s", sc.Fields[0].Name)
	}
	if sc.Fields[1].Name != "created_at" || !sc.Fields[1].System {
		t.Errorf("last should be system created_at, got %+v", sc.Fields[1])
	}
}
