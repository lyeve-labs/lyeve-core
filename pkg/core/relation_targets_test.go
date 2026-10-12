package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestRelationValues_ReadsEveryKeyABelongsToTakes(t *testing.T) {
	fields := []SchemaField{
		{Name: "id", FieldType: "uid", System: true},
		{Name: "title", FieldType: "text"},
		{Name: "author", FieldType: "relation", RelationType: RelBelongsTo, RelationTo: "authors"},
		{Name: "author_id", FieldType: "uid", System: true},
		{Name: "editor", FieldType: "relation", RelationTo: "authors", RelationFKName: "edited_by"},
		{Name: "owner", FieldType: "relation", RelationTo: "authors", RelationFKName: "owner"},
		{Name: "readers", FieldType: "relation", RelationType: RelHasMany, RelationTo: "authors"},
		{Name: "topics", FieldType: "relation", RelationType: RelManyToMany, RelationTo: "topics"},
		{Name: "loose", FieldType: "relation"},
	}
	tests := []struct {
		name string
		data map[string]any
		want []RelationValue
	}{
		{"nothing set", map[string]any{"title": "x"}, nil},
		{"the field name", map[string]any{"author": "a"},
			[]RelationValue{{Field: "author", Target: "authors", Value: "a"}}},
		{"the key column", map[string]any{"author_id": "a"},
			[]RelationValue{{Field: "author_id", Target: "authors", Value: "a"}}},
		{"both keys are both checked", map[string]any{"author": "a", "author_id": "b"},
			[]RelationValue{{Field: "author", Target: "authors", Value: "a"}, {Field: "author_id", Target: "authors", Value: "b"}}},
		{"a declared key column", map[string]any{"edited_by": "e"},
			[]RelationValue{{Field: "edited_by", Target: "authors", Value: "e"}}},
		{"a key column named like the field is read once", map[string]any{"owner": "o"},
			[]RelationValue{{Field: "owner", Target: "authors", Value: "o"}}},
		{"a null is reported for the caller to skip", map[string]any{"author": nil},
			[]RelationValue{{Field: "author", Target: "authors", Value: nil}}},
		{"no value is kept for has_many, many_to_many or a relation with no target",
			map[string]any{"readers": []any{"r"}, "topics": []any{"t"}, "loose": "l"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RelationValues(fields, tt.data))
		})
	}
}

func TestRelationTargetError_IsAValidationFailure(t *testing.T) {
	var err error = &RelationTargetError{Field: "author"}
	assert.True(t, errors.Is(err, ErrValidation))
	assert.False(t, errors.Is(err, ErrNotFound), "a missing target is the request's fault, not a missing resource")
	assert.Equal(t, "author: the related entry does not exist", err.Error())
}

func TestRelationID_ReadsTheShapesWritersSend(t *testing.T) {
	id := uuid.New()
	tests := []struct {
		name             string
		v                any
		want             uuid.UUID
		present, matches bool
	}{
		{"nil clears", nil, uuid.Nil, false, false},
		{"text", id.String(), id, true, true},
		{"upper-case text, as SQL Server writes one", strings.ToUpper(id.String()), id, true, true},
		{"padded text is no id", " " + id.String(), uuid.Nil, true, false},
		{"bytes", []byte(id.String()), id, true, true},
		{"a uuid", id, id, true, true},
		{"a nil pointer clears", (*uuid.UUID)(nil), uuid.Nil, false, false},
		{"a pointer", &id, id, true, true},
		{"a null uuid clears", uuid.NullUUID{}, uuid.Nil, false, false},
		{"an empty string is no id", "", uuid.Nil, true, false},
		{"a number is no id", 42.0, uuid.Nil, true, false},
		{"an object is no id", map[string]any{"id": id.String()}, uuid.Nil, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, present, ok := relationID(tt.v)
			assert.Equal(t, tt.present, present)
			assert.Equal(t, tt.matches, ok)
			if ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestRelationTargetsQuery_LocksWhatItReads(t *testing.T) {
	assert.Equal(t,
		`SELECT id, tenant_id FROM "_authors" WHERE tenant_id = $1 AND id IN ($2, $3) FOR SHARE`,
		relationTargetsQuery("postgres", `"_authors"`, "$2, $3"))
	assert.Equal(t,
		"SELECT id, tenant_id FROM `_authors` WHERE tenant_id = $1 AND id IN ($2) FOR SHARE",
		relationTargetsQuery("mysql", "`_authors`", "$2"))
	assert.Equal(t,
		"SELECT id, tenant_id FROM [_authors] WITH (REPEATABLEREAD, ROWLOCK) WHERE tenant_id = $1 AND id IN ($2)",
		relationTargetsQuery("mssql", "[_authors]", "$2"))
}
