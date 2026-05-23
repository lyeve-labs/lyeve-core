package domain

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTableName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase passthrough", "articles", "_articles"},
		{"uppercase lowered", "Article", "_article"},
		{"dash to underscore", "blog-post", "_blog_post"},
		{"mixed case and dash", "MyBlog-Post", "_myblog_post"},
		{"multiple dashes", "a-b-c", "_a_b_c"},
		{"empty", "", "_"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TableName(tt.in); got != tt.want {
				t.Errorf("TableName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPivotTableName(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want string
	}{
		{"already ordered", "author", "book", "_pivot_author_book"},
		{"reverse ordered swaps deterministically", "book", "author", "_pivot_author_book"},
		{"case-insensitive with swap", "Tag", "Post", "_pivot_post_tag"},
		{"dash normalization", "blog-post", "tag", "_pivot_blog_post_tag"},
		{"identical", "x", "x", "_pivot_x_x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PivotTableName(tt.a, tt.b); got != tt.want {
				t.Errorf("PivotTableName(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// PivotTableName must be order-independent: (a,b) and (b,a) yield the same name.
func TestPivotTableName_Symmetric(t *testing.T) {
	if PivotTableName("author", "book") != PivotTableName("book", "author") {
		t.Error("PivotTableName is not symmetric for (author, book) vs (book, author)")
	}
}

func TestFKColumn(t *testing.T) {
	tests := []struct {
		name     string
		field    string
		override string
		want     string
	}{
		{"derived from field", "author", "", "author_id"},
		{"lowercased", "Author", "", "author_id"},
		{"dash normalized", "blog-post", "", "blog_post_id"},
		{"override wins", "author", "custom_fk", "custom_fk"},
		{"override wins verbatim without normalization", "Author", "AuthorId", "AuthorId"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FKColumn(tt.field, tt.override); got != tt.want {
				t.Errorf("FKColumn(%q, %q) = %q, want %q", tt.field, tt.override, got, tt.want)
			}
		})
	}
}

func TestUserClone_Nil(t *testing.T) {
	var u *User
	if got := u.Clone(); got != nil {
		t.Errorf("(*User)(nil).Clone() = %v, want nil", got)
	}
}

func TestUserClone_Independent(t *testing.T) {
	exp := time.Date(2027, 6, 7, 8, 9, 10, 0, time.UTC)
	orig := &User{
		ID:           uuid.New(),
		Email:        "admin@example.com",
		PasswordHash: "hash",
		Roles:        []string{"admin", "editor"},
		TenantID:     "acme",
		TokenVersion: 3,
		Disabled:     true,
		ExpiresAt:    &exp,
		CreatedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
	}

	clone := orig.Clone()

	if !reflect.DeepEqual(clone, orig) {
		t.Fatalf("clone not equal to original\n got: %+v\nwant: %+v", clone, orig)
	}

	// Mutating the clone's Roles slice must not touch the original.
	clone.Roles[0] = "MUTATED"
	if orig.Roles[0] != "admin" {
		t.Errorf("Roles slice aliased: original mutated to %q", orig.Roles[0])
	}

	// Mutating through the clone's ExpiresAt pointer must not touch the original.
	*clone.ExpiresAt = time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	if !orig.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt pointer aliased: original mutated to %v", orig.ExpiresAt)
	}
	if clone.ExpiresAt == orig.ExpiresAt {
		t.Error("ExpiresAt pointer not deep-copied (same address)")
	}
}

func TestUserClone_NilExpiresAt(t *testing.T) {
	orig := &User{Email: "u@x.io", Roles: nil}
	clone := orig.Clone()
	if clone.ExpiresAt != nil {
		t.Errorf("clone.ExpiresAt = %v, want nil", clone.ExpiresAt)
	}
	if clone.Roles != nil {
		t.Errorf("clone.Roles = %v, want nil", clone.Roles)
	}
	if clone.Email != "u@x.io" {
		t.Errorf("clone.Email = %q, want %q", clone.Email, "u@x.io")
	}
}

func TestPopulateConfig_IsEmpty(t *testing.T) {
	tests := []struct {
		name string
		pc   PopulateConfig
		want bool
	}{
		{"zero value", PopulateConfig{}, true},
		{"empty paths slice", PopulateConfig{Paths: []string{}}, true},
		{"has paths", PopulateConfig{Paths: []string{"author"}}, false},
		{"has max depth", PopulateConfig{MaxDepth: 1}, false},
		{"has both", PopulateConfig{Paths: []string{"a"}, MaxDepth: 2}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pc.IsEmpty(); got != tt.want {
				t.Errorf("PopulateConfig%+v.IsEmpty() = %v, want %v", tt.pc, got, tt.want)
			}
		})
	}
}

func TestRelationFields(t *testing.T) {
	tests := []struct {
		name   string
		fields []SchemaField
		want   []string
	}{
		{"nil fields", nil, nil},
		{"no relations", []SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "count", FieldType: "number"},
		}, nil},
		{"filters relations in order", []SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "author", FieldType: "relation"},
			{Name: "count", FieldType: "number"},
			{Name: "tags", FieldType: "relation"},
		}, []string{"author", "tags"}},
		{"all relations", []SchemaField{
			{Name: "a", FieldType: "relation"},
			{Name: "b", FieldType: "relation"},
		}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RelationFields(tt.fields)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RelationFields() = %v, want %v", got, tt.want)
			}
		})
	}
}
