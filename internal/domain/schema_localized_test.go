package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func schemaWith(f SchemaField) *Schema {
	return &Schema{Name: "article", Fields: []SchemaField{f}}
}

func TestValidateSchema_LocalizedOnFreeText(t *testing.T) {
	for _, ft := range []string{"text", "rich_text", "url"} {
		t.Run(ft, func(t *testing.T) {
			err := ValidateSchema(schemaWith(SchemaField{Name: "body", FieldType: ft, Localized: true}))
			assert.NoError(t, err)
		})
	}
}

// Translating a key breaks the row that points at it. Each of these is refused
// rather than ignored, because a flag that is silently dropped is a flag an
// editor believes in.
func TestValidateSchema_LocalizedRefusedOnEverythingElse(t *testing.T) {
	for _, ft := range []string{"number", "boolean", "date", "datetime", "json", "relation", "media", "uid", "email"} {
		t.Run(ft, func(t *testing.T) {
			err := ValidateSchema(schemaWith(SchemaField{Name: "f", FieldType: ft, Localized: true}))
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrValidation)
		})
	}
}

// A unique field is a key by the tenant's own choosing, and one constraint
// cannot be satisfied in several languages at once.
func TestValidateSchema_LocalizedRefusedOnAUniqueField(t *testing.T) {
	err := ValidateSchema(schemaWith(SchemaField{Name: "slug", FieldType: "text", Unique: true, Localized: true}))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrValidation)
}

func TestValidateSchema_UnmarkedFieldsAreUntouched(t *testing.T) {
	for _, ft := range []string{"number", "relation", "uid", "media"} {
		t.Run(ft, func(t *testing.T) {
			f := SchemaField{Name: "f", FieldType: ft}
			if ft == "relation" {
				f.RelationTo = "tag"
				f.RelationType = RelBelongsTo
			}
			assert.NoError(t, ValidateSchema(schemaWith(f)))
		})
	}
}

// The error has to name the field, or a schema with thirty of them says only
// that one is wrong.
func TestValidateSchema_LocalizedErrorNamesTheField(t *testing.T) {
	err := ValidateSchema(schemaWith(SchemaField{Name: "published_at", FieldType: "datetime", Localized: true}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "published_at")
}

// A system field skips type validation, and the mark is not checked behind
// that exemption either: the loop continues before the type is read at all.
func TestValidateSchema_SystemFieldSkipsTheCheckAsItAlwaysDid(t *testing.T) {
	err := ValidateSchema(schemaWith(SchemaField{Name: "id", FieldType: "uid", System: true, Localized: true}))
	assert.NoError(t, err)
}

// The mark travels in the definition JSON under the name the admin and the
// translation panel read. An unmarked field leaves the key out, so a stored
// definition that never used it reads back byte for byte unchanged.
func TestSchemaField_LocalizedWireName(t *testing.T) {
	raw, err := json.Marshal(SchemaField{Name: "title", FieldType: "text", Localized: true})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"localized":true`)

	raw, err = json.Marshal(SchemaField{Name: "title", FieldType: "text"})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "localized")

	var back SchemaField
	require.NoError(t, json.Unmarshal([]byte(`{"name":"title","field_type":"text","localized":true}`), &back))
	assert.True(t, back.Localized)
}
