package schemaimport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

func TestParseBundle_ReadsYAML(t *testing.T) {
	b, err := ParseBundle([]byte(`
version: 1
schemas:
  - name: article
    display_name: Article
    fields:
      - name: title
        field_type: text
        required: true
`))
	require.NoError(t, err)

	require.Len(t, b.Schemas, 1)
	assert.Equal(t, "article", b.Schemas[0].Name)
	require.Len(t, b.Schemas[0].Fields, 1)
	assert.Equal(t, "text", b.Schemas[0].Fields[0].FieldType)
	assert.True(t, b.Schemas[0].Fields[0].Required)
}

func TestParseBundle_ReadsJSON(t *testing.T) {
	b, err := ParseBundle([]byte(
		`{"version":1,"schemas":[{"name":"article","display_name":"Article",
		  "fields":[{"name":"title","field_type":"text"}]}]}`))
	require.NoError(t, err)
	assert.Equal(t, "article", b.Schemas[0].Name)
}

func TestParseBundle_DefaultsTheVersion(t *testing.T) {
	b, err := ParseBundle([]byte("schemas:\n  - name: a\n    fields:\n      - {name: f, field_type: text}\n"))
	require.NoError(t, err)
	assert.Equal(t, BundleVersion, b.Version)
}

func TestParseBundle_RejectsANewerVersion(t *testing.T) {
	// Reading it anyway would drop the fields this build does not know and the
	// import would look like it worked.
	_, err := ParseBundle([]byte("version: 99\nschemas:\n  - name: a\n    fields: []\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than this engine understands")
}

func TestParseBundle_RejectsAMisspelledKey(t *testing.T) {
	// A bundle whose "fields" is spelled "field" would import as a table with
	// no columns.
	_, err := ParseBundle([]byte("schemas:\n  - name: a\n    field:\n      - {name: f, field_type: text}\n"))
	require.Error(t, err)
}

func TestParseBundle_RejectsDuplicateSchemas(t *testing.T) {
	_, err := ParseBundle([]byte(`
schemas:
  - name: article
    fields: [{name: a, field_type: text}]
  - name: article
    fields: [{name: b, field_type: text}]
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared twice")
}

func TestParseBundle_RejectsAnInvalidSchema(t *testing.T) {
	// Reported against the file rather than part-way through an import that has
	// already created tables.
	_, err := ParseBundle([]byte("schemas:\n  - name: article\n    fields:\n      - {name: title, field_type: nonsense}\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "article")
}

func TestParseBundle_RejectsEmptyInput(t *testing.T) {
	for _, in := range []string{"", "# just a comment\n", "schemas: []\n"} {
		_, err := ParseBundle([]byte(in))
		assert.Error(t, err, "input %q", in)
	}
}

func TestBundle_MarshalYAMLRoundTrips(t *testing.T) {
	original := &Bundle{
		Version: BundleVersion,
		Schemas: []domain.Schema{{
			Name:        "article",
			DisplayName: "Article",
			Fields: []domain.SchemaField{
				{Name: "title", FieldType: "text", Required: true},
				{Name: "author", FieldType: "relation", RelationTo: "person", RelationType: domain.RelBelongsTo},
			},
		}},
	}

	out, err := original.MarshalYAML()
	require.NoError(t, err)

	back, err := ParseBundle(out)
	require.NoError(t, err)
	assert.Equal(t, original.Schemas, back.Schemas)
}

func relationField(name, to, kind string) domain.SchemaField {
	return domain.SchemaField{Name: name, FieldType: "relation", RelationTo: to, RelationType: kind}
}

func TestDependencies(t *testing.T) {
	tests := []struct {
		name  string
		field domain.SchemaField
		want  []string
	}{
		{name: "belongs_to holds the key here", field: relationField("a", "author", domain.RelBelongsTo), want: []string{"author"}},
		{name: "many_to_many builds a pivot referencing both", field: relationField("a", "tag", domain.RelManyToMany), want: []string{"tag"}},
		{name: "has_many holds no column here", field: relationField("a", "comment", domain.RelHasMany)},
		{name: "has_one holds no column here", field: relationField("a", "profile", domain.RelHasOne)},
		{name: "a plain field is not a dependency", field: domain.SchemaField{Name: "a", FieldType: "text"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Dependencies(domain.Schema{Name: "post", Fields: []domain.SchemaField{tc.field}})
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDependencies_IgnoresSelfReference(t *testing.T) {
	// A table referencing itself needs nothing else to exist first.
	got := Dependencies(domain.Schema{
		Name:   "page",
		Fields: []domain.SchemaField{relationField("parent", "page", domain.RelBelongsTo)},
	})
	assert.Empty(t, got)
}

func TestDependencies_DeduplicatesAndSorts(t *testing.T) {
	got := Dependencies(domain.Schema{Name: "post", Fields: []domain.SchemaField{
		relationField("editor", "person", domain.RelBelongsTo),
		relationField("author", "person", domain.RelBelongsTo),
		relationField("cat", "category", domain.RelBelongsTo),
	}})
	assert.Equal(t, []string{"category", "person"}, got)
}

func TestBundle_OrderedPutsReferencedSchemasFirst(t *testing.T) {
	// Applying in file order fails as soon as a belongs_to points at a table
	// that does not exist yet.
	b := &Bundle{Schemas: []domain.Schema{
		{Name: "article", Fields: []domain.SchemaField{relationField("author", "person", domain.RelBelongsTo)}},
		{Name: "person", Fields: []domain.SchemaField{{Name: "name", FieldType: "text"}}},
	}}

	ordered, cyclic := b.Ordered()
	require.Len(t, ordered, 2)
	assert.Equal(t, "person", ordered[0].Name)
	assert.Equal(t, "article", ordered[1].Name)
	assert.Empty(t, cyclic)
}

func TestBundle_OrderedHandlesAChain(t *testing.T) {
	b := &Bundle{Schemas: []domain.Schema{
		{Name: "c", Fields: []domain.SchemaField{relationField("b", "b", domain.RelBelongsTo)}},
		{Name: "b", Fields: []domain.SchemaField{relationField("a", "a", domain.RelBelongsTo)}},
		{Name: "a", Fields: []domain.SchemaField{{Name: "n", FieldType: "text"}}},
	}}

	ordered, cyclic := b.Ordered()
	var names []string
	for _, s := range ordered {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{"a", "b", "c"}, names)
	assert.Empty(t, cyclic)
}

func TestBundle_OrderedReportsACycle(t *testing.T) {
	// Neither table can carry its constraint on the first pass.
	b := &Bundle{Schemas: []domain.Schema{
		{Name: "a", Fields: []domain.SchemaField{relationField("b", "b", domain.RelBelongsTo)}},
		{Name: "b", Fields: []domain.SchemaField{relationField("a", "a", domain.RelBelongsTo)}},
	}}

	ordered, cyclic := b.Ordered()
	assert.Len(t, ordered, 2, "both are still emitted")
	assert.NotEmpty(t, cyclic, "the cycle is reported so the importer applies a second pass")
}

func TestBundle_OrderedKeepsSchemasReferencingOutsideTheBundle(t *testing.T) {
	// Refusing here would block importing one part of a project at a time.
	b := &Bundle{Schemas: []domain.Schema{
		{Name: "article", Fields: []domain.SchemaField{relationField("author", "not_in_bundle", domain.RelBelongsTo)}},
	}}

	ordered, _ := b.Ordered()
	require.Len(t, ordered, 1)
	assert.Equal(t, "article", ordered[0].Name)
}

func TestBundle_OrderedIsDeterministic(t *testing.T) {
	b := &Bundle{Schemas: []domain.Schema{
		{Name: "zebra", Fields: []domain.SchemaField{{Name: "n", FieldType: "text"}}},
		{Name: "alpha", Fields: []domain.SchemaField{{Name: "n", FieldType: "text"}}},
		{Name: "mike", Fields: []domain.SchemaField{{Name: "n", FieldType: "text"}}},
	}}

	first, _ := b.Ordered()
	for i := 0; i < 5; i++ {
		again, _ := b.Ordered()
		assert.Equal(t, first, again, "unrelated schemas order by name, not by map iteration")
	}
}

// Every schema on a reference cycle is named, not only the one the closing
// edge reaches. Each references the next, so any member applied with its
// relations first names a table that does not exist yet.
func TestOrdered_NamesEveryMemberOfACycle(t *testing.T) {
	b := &Bundle{Schemas: []domain.Schema{
		{Name: "a", Fields: []domain.SchemaField{relationField("b", "b", domain.RelBelongsTo)}},
		{Name: "b", Fields: []domain.SchemaField{relationField("c", "c", domain.RelManyToMany)}},
		{Name: "c", Fields: []domain.SchemaField{relationField("a", "a", domain.RelBelongsTo)}},
		{Name: "d", Fields: []domain.SchemaField{relationField("a", "a", domain.RelBelongsTo)}},
		{Name: "e", Fields: []domain.SchemaField{relationField("parent", "e", domain.RelBelongsTo)}},
	}}
	_, cyclic := b.Ordered()
	assert.Equal(t, []string{"a", "b", "c"}, cyclic)
}

// The first pass keeps every column that is not a reference to another
// schema, including a relation to the schema itself.
func TestFirstPass_LeavesOutReferencesToOtherSchemas(t *testing.T) {
	s := domain.Schema{Name: "post", Fields: []domain.SchemaField{
		{Name: "title", FieldType: "text"},
		relationField("author", "person", domain.RelBelongsTo),
		relationField("tags", "tag", domain.RelManyToMany),
		relationField("parent", "post", domain.RelBelongsTo),
	}}
	got := FirstPass(s)
	var names []string
	for _, f := range got.Fields {
		names = append(names, f.Name)
	}
	assert.Equal(t, []string{"title", "parent"}, names)
	assert.Len(t, s.Fields, 4, "the bundle's own definition is left as it was")
}
