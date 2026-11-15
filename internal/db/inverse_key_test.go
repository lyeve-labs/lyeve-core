package db

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// The key a has_one or has_many on posts toward comments reads, for each
// row of the rule.
func TestInverseKey_FollowsTheRule(t *testing.T) {
	belongsTo := func(name, to, key string) domain.SchemaField {
		return domain.SchemaField{Name: name, FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: to, RelationFKName: key}
	}
	text := func(name string) domain.SchemaField {
		return domain.SchemaField{Name: name, FieldType: "text"}
	}
	tests := []struct {
		name   string
		source string
		key    string
		target []domain.SchemaField
		want   string
		ok     bool
	}{
		{"one belongs_to back", "posts", "", []domain.SchemaField{belongsTo("post", "posts", "")}, "post_id", true},
		{"a belongs_to back that names its column", "posts", "", []domain.SchemaField{belongsTo("post", "posts", "parent_ref")}, "parent_ref", true},
		{"the key names one of two belongs_to back", "posts", "reply_to_id", []domain.SchemaField{belongsTo("post", "posts", ""), belongsTo("reply_to", "posts", "")}, "reply_to_id", true},
		{"two belongs_to back and no key", "posts", "", []domain.SchemaField{belongsTo("post", "posts", ""), belongsTo("reply_to", "posts", "")}, "", false},
		{"the key is no belongs_to back", "posts", "author_id", []domain.SchemaField{belongsTo("post", "posts", ""), text("author_id")}, "", false},
		{"the key is a belongs_to toward another type", "posts", "author_id", []domain.SchemaField{belongsTo("author", "people", "")}, "", false},
		{"a column named for the source type", "posts", "", []domain.SchemaField{text("posts_id")}, "posts_id", true},
		{"a belongs_to elsewhere whose column is named for the source type", "posts", "", []domain.SchemaField{belongsTo("posts", "archive", "")}, "posts_id", true},
		{"nothing points back", "posts", "", []domain.SchemaField{text("body")}, "", false},
		{"a belongs_to toward another type is not read", "posts", "", []domain.SchemaField{belongsTo("author", "people", "")}, "", false},
		{"a system column is never the key", "tenant", "", []domain.SchemaField{text("body"), {Name: "tenant_id", FieldType: "text", System: true}}, "", false},
		{"an inverse field named for the source type keeps no column", "posts", "", []domain.SchemaField{{Name: "posts_id", FieldType: "relation", RelationType: domain.RelHasMany, RelationTo: "posts"}}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := domain.SchemaField{Name: "comments", FieldType: "relation", RelationType: domain.RelHasMany, RelationTo: "comments", RelationFKName: tt.key}
			got, ok := inverseKey(tt.source, f, &domain.Schema{Name: "comments", Fields: tt.target})
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
