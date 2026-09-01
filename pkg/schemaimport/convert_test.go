package schemaimport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

func TestIdentifier(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"title", "title"},
		{"publishedAt", "published_at"},
		{"PublishedAt", "published_at"},
		{"HTTPStatus", "http_status"},
		{"my-field", "my_field"},
		{"my field", "my_field"},
		{"My Field Name", "my_field_name"},
		{"field__with___runs", "field_with_runs"},
		{"_leading", "leading"},
		{"trailing_", "trailing"},
		{"2fast", "f_2fast"},
		{"café", "caf"},
		{"...", ""},
		{"", ""},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, Identifier(tc.in), "Identifier(%q)", tc.in)
	}
}

func TestIdentifier_TruncatesToTheColumnLimit(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	got := Identifier(long)
	assert.Len(t, got, 63)
	assert.NoError(t, domain.ValidateIdentifier(got))
}

func TestFieldName_AvoidsTheEnginesOwnColumns(t *testing.T) {
	// Landing on id or created_at would collide with a column the engine
	// manages.
	for _, reserved := range []string{"id", "created_at", "updated_at", "deleted_at"} {
		got := fieldName(reserved, map[string]bool{})
		assert.NotEqual(t, reserved, got, "%q must not be used as-is", reserved)
		assert.Equal(t, reserved+"_field", got)
	}
}

func TestFieldName_DisambiguatesCollisions(t *testing.T) {
	// Two source names can fold to the same identifier, and the second must not
	// silently replace the first.
	taken := map[string]bool{}
	first := fieldName("my-field", taken)
	taken[first] = true
	second := fieldName("my field", taken)

	assert.Equal(t, "my_field", first)
	assert.Equal(t, "my_field_2", second)
}

func TestBuilder_RecordsRenames(t *testing.T) {
	conv := &Conversion{Bundle: &Bundle{}}
	b := newBuilder(conv, "BlogPost", "")
	b.add("publishedAt", domain.SchemaField{FieldType: "datetime"})

	assert.Equal(t, "blog_post", b.schema.Name)
	assert.Equal(t, "Blog Post", b.schema.DisplayName)
	assert.Equal(t, "BlogPost -> blog_post", conv.Renamed["blog_post"])
	assert.Equal(t, "publishedAt -> published_at", conv.Renamed["blog_post.published_at"])
}

func TestBuilder_DropsAnUnusableName(t *testing.T) {
	conv := &Conversion{Bundle: &Bundle{}}
	b := newBuilder(conv, "post", "")
	got := b.add("...", domain.SchemaField{FieldType: "text"})

	assert.Equal(t, "", got)
	assert.Empty(t, b.schema.Fields)
	require.Len(t, conv.Notes, 1)
	assert.Contains(t, conv.Notes[0].Message, "no characters usable in an identifier")
}

func TestDisplayName(t *testing.T) {
	assert.Equal(t, "Blog Post", displayName("blogPost"))
	assert.Equal(t, "Article", displayName("article"))
	assert.Equal(t, "My Content Type", displayName("my-content-type"))
}

// fieldByName is a test helper: converters order fields by source name, so a
// test that indexes by position breaks whenever a mapping changes.
func fieldByName(t *testing.T, s domain.Schema, name string) domain.SchemaField {
	t.Helper()
	for _, f := range s.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("schema %q has no field %q (has %v)", s.Name, name, fieldNames(s))
	return domain.SchemaField{}
}

func fieldNames(s domain.Schema) []string {
	var out []string
	for _, f := range s.Fields {
		out = append(out, f.Name)
	}
	return out
}

func schemaByName(t *testing.T, b *Bundle, name string) domain.Schema {
	t.Helper()
	for _, s := range b.Schemas {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("bundle has no schema %q", name)
	return domain.Schema{}
}
