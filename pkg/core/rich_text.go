package core

import "encoding/json"

// The formats a rich_text field stores, named by SchemaField.Format.
const (
	// RichTextFormatHTML stores cleaned HTML as text. It is what an empty
	// Format means.
	RichTextFormatHTML = "html"
	// RichTextFormatDocument stores a structured document with the HTML drawn
	// from it, as the JSON text of an object holding "document" and "html".
	RichTextFormatDocument = "document"
)

// RichTextValue answers a stored value in the form a read returns it. A
// document field's stored JSON text is decoded into its object, so a reader
// gets { "document": ..., "html": "..." } rather than a string holding it.
// Every other value, an HTML field's included, is answered as it is, and so is
// text that does not decode to a JSON object.
func RichTextValue(f SchemaField, v any) any {
	if f.FieldType != "rich_text" || f.Format != RichTextFormatDocument {
		return v
	}
	str, ok := v.(string)
	if !ok {
		return v
	}
	var obj map[string]any
	if json.Unmarshal([]byte(str), &obj) != nil || obj == nil {
		return v
	}
	return obj
}
