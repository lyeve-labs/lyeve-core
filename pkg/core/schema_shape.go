package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Schema defines the structure of a content type.
//
// This is the definition only: the fields a content type declares and the
// naming conventions its storage follows. Generating or applying DDL from a
// definition is the schema engine's work and is reached through SchemaEngine.
type Schema struct {
	Name             string        `json:"name"`
	DisplayName      string        `json:"display_name"`
	Fields           []SchemaField `json:"fields"`
	WithCreatedAt    bool          `json:"with_created_at,omitempty"`
	WithUpdatedAt    bool          `json:"with_updated_at,omitempty"`
	WithSoftDelete   bool          `json:"with_soft_delete,omitempty"`   // adds deleted_at TIMESTAMPTZ. Delete soft-deletes
	WithDraftPublish bool          `json:"with_draft_publish,omitempty"` // adds _status TEXT DEFAULT 'published'
	WithLocalization bool          `json:"with_localization,omitempty"`  // adds _locale TEXT DEFAULT 'en'

	// Transports says which transports serve this schema and whether they may
	// write through it. Absent, or an absent key inside it, is read-write, so
	// a definition that never mentions transports is served read-write
	// everywhere. See SchemaTransports.
	Transports SchemaTransports `json:"transports,omitempty"`

	// CrossFieldValidation is a list of cross-field validation rules.
	// Each rule operates on two or more fields, for example "date_to must be
	// greater than or equal to date_from".
	CrossFieldValidation []CrossFieldRule `json:"cross_field_validation,omitempty"`

	// CustomValidators registers user-defined validation functions by name.
	// Custom rules in ValidationRule reference these names via rule="custom:<name>".
	CustomValidators []CustomValidator `json:"custom_validators,omitempty"`
}

// SchemaField is one column or property within a Schema.
type SchemaField struct {
	Name      string `json:"name"`
	FieldType string `json:"field_type"` // text, number, boolean, datetime, json, relation, media, email, uid
	Required  bool   `json:"required"`
	Unique    bool   `json:"unique"`
	Indexed   bool   `json:"indexed"`
	Default   any    `json:"default,omitempty"`

	// Validation rules applied when content is created or updated.
	// Only enforced when the field holds a value, unless Required.
	Validation []ValidationRule `json:"validation,omitempty"`

	// Relation fields
	RelationTo   string `json:"relation_to,omitempty"`   // referenced schema name
	RelationType string `json:"relation_type,omitempty"` // belongs_to | has_one | has_many | many_to_many
	// RelationThrough overrides the auto-derived pivot table name for many_to_many.
	RelationThrough string `json:"relation_through,omitempty"`
	// RelationFKName overrides the auto-derived FK column name for belongs_to and has_one.
	RelationFKName string `json:"relation_fk_name,omitempty"`

	// System marks fields managed by the engine, such as id, which users cannot edit.
	System bool `json:"system,omitempty"`

	// Localized marks a field whose value is translated per locale.
	//
	// It generates no DDL. A translation is stored apart from the content
	// table, one row per entry and locale, so marking a field changes no
	// column and runs no migration against tenant data. What it changes is
	// what an editor is offered: the translation panel renders exactly the
	// fields that carry the mark, and completeness is counted against them.
	//
	// LocalizedRefusal bounds it, and every validator refuses a field it
	// names rather than dropping the mark.
	Localized bool `json:"localized,omitempty"`
}

// ValidationRule is a single field-level constraint.
// Rule is the validator name, such as "email", "url", "regex", "enum", "min"
// or "max". Params carries rule-specific parameters.
type ValidationRule struct {
	Rule   string         `json:"rule"`
	Params map[string]any `json:"params,omitempty"`
	// Message overrides the default i18n error message key.
	Message string `json:"message,omitempty"`
}

// CrossFieldRule is a validation rule that references multiple fields.
// Rule is the validator name, such as "required_with", "lt_field" or
// "gte_field". Targets lists the field names this rule depends on.
type CrossFieldRule struct {
	Rule    string         `json:"rule"`
	Params  map[string]any `json:"params,omitempty"`
	Targets []string       `json:"targets"`
	Message string         `json:"message,omitempty"`
}

// CustomValidator is a user-defined validation function registered at runtime.
// Name is a unique key used to look up the function, and Body is the
// validation expression a registered handler interprets.
type CustomValidator struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// RelationType describes the cardinality of a relation field.
//
//   - belongs_to: this table holds the FK column, {field}_id UUID
//   - has_one: the inverse of belongs_to, with no column on this table
//   - has_many: one-to-many, with no column on this table
//   - many_to_many: joined through the pivot table _pivot_{a}_{b}
type RelationType = string

const (
	// RelBelongsTo marks a field that holds the foreign key column.
	RelBelongsTo RelationType = "belongs_to"
	// RelHasOne marks a field that is the inverse of belongs_to.
	RelHasOne RelationType = "has_one"
	// RelHasMany marks a one-to-many relation field.
	RelHasMany RelationType = "has_many"
	// RelManyToMany marks a many-to-many relation via a pivot table.
	RelManyToMany RelationType = "many_to_many"
)

// PivotTableName returns the canonical pivot table name for a many-to-many
// relation between two schemas. The names are ordered alphabetically so the
// result does not depend on which side asks.
func PivotTableName(a, b string) string {
	an := strings.ToLower(strings.ReplaceAll(a, "-", "_"))
	bn := strings.ToLower(strings.ReplaceAll(b, "-", "_"))
	if an > bn {
		an, bn = bn, an
	}
	return "_pivot_" + an + "_" + bn
}

// PivotNameLimit is the longest pivot table name a new many_to_many is given.
// PostgreSQL keeps 63 bytes of a name and cuts the rest without an error, so
// two long names sharing their first 63 bytes would name one table and mix
// two relations' pairs in it. MySQL refuses a name past 64 characters. The
// strictest limit holds on every dialect, so a definition names the same
// table wherever it is applied.
const PivotNameLimit = 63

// pivotHashLen is how many hex characters of the hash a bounded name keeps.
const pivotHashLen = 8

// BoundedPivotTableName returns the name a pivot between a and b is created
// under when no table holds it yet: PivotTableName when that fits
// PivotNameLimit, and otherwise its first PivotNameLimit-9 characters, an
// underscore and the first 8 hex characters of its SHA-256. The same pair
// always bounds to the same name, and two long pairs that share a prefix
// still differ. Type names are ASCII, so characters and bytes count alike.
func BoundedPivotTableName(a, b string) string {
	natural := PivotTableName(a, b)
	if len(natural) <= PivotNameLimit {
		return natural
	}
	sum := sha256.Sum256([]byte(natural))
	return natural[:PivotNameLimit-pivotHashLen-1] + "_" + hex.EncodeToString(sum[:])[:pivotHashLen]
}

// ResolvePivotTable returns the table a many_to_many between a and b keeps
// its pairs in. A PivotTableName that fits PivotNameLimit is that table. A
// longer one is kept when present reports a table under it, because an
// install made before pivot names were bounded may hold one (MySQL at 64
// characters, SQL Server up to 128, PostgreSQL under the 63 bytes it cut the
// name to), and is BoundedPivotTableName otherwise. The schema engine and the
// content store both resolve through it, so they name one table. present is
// asked only about a name past the limit.
func ResolvePivotTable(a, b string, present func(name string) (bool, error)) (string, error) {
	natural := PivotTableName(a, b)
	if len(natural) <= PivotNameLimit {
		return natural, nil
	}
	ok, err := present(natural)
	if err != nil {
		return "", err
	}
	if ok {
		return natural, nil
	}
	return BoundedPivotTableName(a, b), nil
}

// FKColumn returns the FK column name for a belongs_to or has_one field.
// A non-empty override is used directly, otherwise the name is "{fieldName}_id".
func FKColumn(fieldName, override string) string {
	if override != "" {
		return override
	}
	return strings.ToLower(strings.ReplaceAll(fieldName, "-", "_")) + "_id"
}

// TableName returns the physical table name for a schema. Every
// per-collection table carries a leading underscore: _{name}. The engine's own
// sys_ tables are not affected.
func TableName(schemaName string) string {
	return "_" + strings.ToLower(strings.ReplaceAll(schemaName, "-", "_"))
}

// PopulateConfig defines how to populate relation fields on content responses.
// It is built from the query parameters populate and depth, then passed to the
// content store for execution.
type PopulateConfig struct {
	// Paths lists explicit relation fields to populate. Dot-notation nests:
	// "author", "author.avatar", "category.tags". "*" populates every relation
	// field at the current level, and "field.*" populates every nested
	// relation within a populated field.
	Paths []string

	// MaxDepth auto-populates relation fields up to this depth. The default of
	// 0 populates only the explicitly requested paths.
	MaxDepth int
}

// IsEmpty reports whether no population is configured.
func (pc PopulateConfig) IsEmpty() bool {
	return len(pc.Paths) == 0 && pc.MaxDepth == 0
}

// RelationFields returns the names of every relation-type field in a schema.
func RelationFields(fields []SchemaField) []string {
	var out []string
	for _, f := range fields {
		if f.FieldType == "relation" {
			out = append(out, f.Name)
		}
	}
	return out
}
