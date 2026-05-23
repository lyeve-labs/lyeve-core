package core

import "fmt"

// localizableFieldTypes is the set a field may be marked Localized on.
//
// Free text and nothing else. A translated number is a different number, a
// translated date is a different instant, and a translated relation, media
// reference or uid points at something that does not exist. Each of those is
// a key or a value the rest of the row depends on.
var localizableFieldTypes = map[string]bool{
	"text":      true,
	"rich_text": true,
	"url":       true,
}

// localizableList names localizableFieldTypes in a stable order for messages.
const localizableList = "rich_text, text, url"

// IsLocalizableFieldType reports whether a field of this type may carry the
// Localized mark.
func IsLocalizableFieldType(fieldType string) bool {
	return localizableFieldTypes[fieldType]
}

// LocalizedRefusal returns why f may not carry the Localized mark, or the
// empty string when it may or when it does not carry it.
//
// A validator refuses the field rather than clearing the mark. A flag that is
// silently dropped is a flag an editor believes in, and they would go on
// filling a panel whose contents reach nobody. The reason carries no sentinel,
// so each validator wraps it in its own.
func LocalizedRefusal(f SchemaField) string {
	if !f.Localized {
		return ""
	}
	if !localizableFieldTypes[f.FieldType] {
		return fmt.Sprintf("field_type %q cannot be localized (allowed: %s)", f.FieldType, localizableList)
	}
	// A unique field is a key by the tenant's own choosing, and a per-locale
	// value cannot satisfy one constraint in several languages at once.
	if f.Unique {
		return "a unique field cannot be localized"
	}
	return ""
}
