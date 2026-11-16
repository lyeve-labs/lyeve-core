package core

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRichTextValue_DecodesOnlyADocumentFieldsObject(t *testing.T) {
	document := SchemaField{Name: "body", FieldType: "rich_text", Format: RichTextFormatDocument}
	html := SchemaField{Name: "body", FieldType: "rich_text"}
	explicitHTML := SchemaField{Name: "body", FieldType: "rich_text", Format: RichTextFormatHTML}
	text := SchemaField{Name: "body", FieldType: "text", Format: RichTextFormatDocument}
	stored := `{"document":{"type":"doc"},"html":"<p>a</p>"}`

	cases := []struct {
		name  string
		field SchemaField
		value any
		want  any
	}{
		{"document text reads as its object", document, stored,
			map[string]any{"document": map[string]any{"type": "doc"}, "html": "<p>a</p>"}},
		{"document holding html stays text", document, "<p>old</p>", "<p>old</p>"},
		{"document holding a json array stays text", document, `[1,2]`, `[1,2]`},
		{"document holding json null stays text", document, `null`, `null`},
		{"document already an object is kept", document, map[string]any{"html": "x"}, map[string]any{"html": "x"}},
		{"document null stays null", document, nil, nil},
		{"html field is untouched", html, stored, stored},
		{"explicit html field is untouched", explicitHTML, stored, stored},
		{"a format on another type is ignored", text, stored, stored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RichTextValue(tc.field, tc.value))
		})
	}
}

// The option is new, so a definition stored without it must encode exactly
// as it did: an empty format is left out.
func TestSchemaField_EmptyFormatIsLeftOutOfTheDefinition(t *testing.T) {
	b, err := json.Marshal(SchemaField{Name: "body", FieldType: "rich_text"})
	assert.NoError(t, err)
	assert.NotContains(t, string(b), "format")

	b, err = json.Marshal(SchemaField{Name: "body", FieldType: "rich_text", Format: RichTextFormatDocument})
	assert.NoError(t, err)
	assert.Contains(t, string(b), `"format":"document"`)
}
