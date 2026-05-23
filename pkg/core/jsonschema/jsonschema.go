// Package jsonschema generates JSON Schema (Draft-07) from Go struct types via
// reflection. It reads `json` and `jsonschema` struct tags to produce schemas
// the admin dashboard uses for auto-generating config forms.
//
// Tag reference:
//
//	json:"name,omitempty" - field name and optionality
//	jsonschema:"desc=..." - description
//	jsonschema:"title=..." - custom title (default: json name)
//	jsonschema:"enum=..." / "default=..." / "min=..." / "max=..."
//	jsonschema:"pattern=..." / "format=..." / "deprecated" / "readonly"
package jsonschema

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Generate produces a JSON Schema document for the given Go struct type.
// Returns nil if t is not a struct.
func Generate(t reflect.Type) map[string]any {
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}

	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	required := make([]string, 0)

	walkStruct(t, schema["properties"].(map[string]any), &required)

	if len(required) > 0 {
		schema["required"] = required
	}

	return schema
}

func walkStruct(t reflect.Type, props map[string]any, required *[]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		// Handle embedded structs first: the implicit field name of an
		// embedded struct is the type name, which may be unexported even
		// though the type's own fields are exported. Checking Anonymous
		// before IsExported avoids silently skipping embedded structs
		// whose type name happens to start with a lowercase letter.
		if f.Anonymous {
			if f.Type.Kind() == reflect.Struct {
				walkStruct(f.Type, props, required)
			}
			continue
		}

		// Skip unexported fields.
		if !f.IsExported() {
			continue
		}

		jsonTag := f.Tag.Get("json")
		name, opts := parseJSONTag(jsonTag)
		if name == "-" {
			continue
		}
		if name == "" {
			// Use lowercase field name as fallback.
			name = strings.ToLower(f.Name[:1]) + f.Name[1:]
		}

		prop := fieldSchema(f)
		if prop == nil {
			continue
		}

		// Read jsonschema tag overrides.
		jsTag := f.Tag.Get("jsonschema")
		applyJSTag(prop, jsTag)

		props[name] = prop

		// Required unless json:",omitempty" or jsonschema says optional.
		if !opts.omitempty && !jsTagHas(jsTag, "optional") {
			*required = append(*required, name)
		}
	}
}

type jsonOpts struct {
	omitempty bool
}

func parseJSONTag(tag string) (name string, opts jsonOpts) {
	if tag == "" {
		return "", jsonOpts{}
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "omitempty" {
			opts.omitempty = true
		}
	}
	return
}

func fieldSchema(f reflect.StructField) map[string]any {
	ft := f.Type
	nullable := false
	if ft.Kind() == reflect.Ptr {
		nullable = true
		ft = ft.Elem()
	}

	// json.RawMessage is []byte. Detect early so it doesn't get caught
	// by the reflect.Slice case and returned as {"type":"array","items":{}}.
	if ft == reflect.TypeOf(json.RawMessage{}) {
		return map[string]any{}
	}

	if nullable {
		elem := fieldSchemaForType(ft, f)
		if elem == nil {
			return nil
		}
		return map[string]any{
			"oneOf": []any{
				elem,
				map[string]any{"type": "null"},
			},
		}
	}

	return fieldSchemaForType(ft, f)
}

func fieldSchemaForType(ft reflect.Type, f reflect.StructField) map[string]any {
	// Detect uuid.UUID (which is type UUID [16]byte, so reflect.Kind is Array,
	// not String) before the general kind switch so it gets {type:string,format:uuid}
	// rather than falling into the default branch.
	if ft == reflect.TypeOf(uuid.UUID{}) {
		return map[string]any{
			"type":      "string",
			"format":    "uuid",
			"minLength": 36,
			"maxLength": 36,
		}
	}

	// json.RawMessage is caught earlier in fieldSchema, but also guard here
	// in case it reaches the Slice branch via an unexported field's sub-type.
	if ft == reflect.TypeOf(json.RawMessage{}) {
		return map[string]any{}
	}

	switch ft.Kind() {
	case reflect.String:
		return stringSchema(ft, f)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}

	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}

	case reflect.Bool:
		return map[string]any{"type": "boolean"}

	case reflect.Slice:
		elem := fieldSchemaForType(ft.Elem(), reflect.StructField{})
		if elem == nil {
			elem = map[string]any{}
		}
		return map[string]any{
			"type":  "array",
			"items": elem,
		}

	case reflect.Struct:
		return structSchema(ft)

	case reflect.Interface:
		// json.RawMessage, any and the like accept anything.
		return map[string]any{}

	default:
		// Unsupported: return basic type.
		return map[string]any{"type": ft.Kind().String()}
	}
}

func stringSchema(ft reflect.Type, f reflect.StructField) map[string]any {
	s := map[string]any{"type": "string"}

	// Detect time.Time.
	if ft == reflect.TypeOf(time.Time{}) {
		s["format"] = "date-time"
		return s
	}

	// Detect uuid.UUID.
	if ft == reflect.TypeOf(uuid.UUID{}) {
		s["format"] = "uuid"
		s["minLength"] = 36
		s["maxLength"] = 36
		return s
	}

	return s
}

func structSchema(ft reflect.Type) map[string]any {
	// json.RawMessage is a []byte type, already handled above.

	s := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	required := make([]string, 0)
	walkStruct(ft, s["properties"].(map[string]any), &required)
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// jsonschema tag parsing

func applyJSTag(prop map[string]any, tag string) {
	if tag == "" {
		return
	}
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if part == "" || part == "deprecated" || part == "readonly" || part == "optional" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch key {
		case "desc":
			prop["description"] = val
		case "title":
			prop["title"] = val
		case "enum":
			enums := strings.Split(val, ",")
			vals := make([]any, len(enums))
			for i, e := range enums {
				vals[i] = strings.TrimSpace(e)
			}
			prop["enum"] = vals
		case "default":
			prop["default"] = parseLiteral(val)
		case "min":
			applyNumericConstraint(prop, "minimum", strings.TrimPrefix(key, "min"), val)
			applyStringConstraint(prop, "minLength", val)
		case "max":
			applyNumericConstraint(prop, "maximum", strings.TrimPrefix(key, "max"), val)
			applyStringConstraint(prop, "maxLength", val)
		case "pattern":
			prop["pattern"] = val
		case "format":
			prop["format"] = val
		}
	}
}

func jsTagHas(tag, key string) bool {
	for _, part := range strings.Split(tag, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, key+"=") || part == key {
			return true
		}
	}
	return false
}

func applyNumericConstraint(prop map[string]any, jsonKey, _ string, val string) {
	n, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return
	}
	prop[jsonKey] = n
}

func applyStringConstraint(prop map[string]any, jsonKey, val string) {
	n, err := strconv.Atoi(val)
	if err != nil {
		return
	}
	// Only apply minLength/maxLength to string types.
	if prop["type"] == "string" {
		prop[jsonKey] = n
	}
}

func parseLiteral(s string) any {
	switch s {
	case "null":
		return nil
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n
	}
	return s
}

// Options configures schema generation behavior.
type Options struct {
	// Title overrides the schema title.
	Title string
	// Description sets the top-level schema description.
	Description string
	// ReadOnly marks all fields as read-only (for display-only schemas).
	ReadOnly bool
}

// GenerateWithOptions produces a JSON Schema with top-level metadata.
func GenerateWithOptions(t reflect.Type, opts Options) map[string]any {
	s := Generate(t)
	if s == nil {
		return nil
	}
	if opts.Title != "" {
		s["title"] = opts.Title
	}
	if opts.Description != "" {
		s["description"] = opts.Description
	}
	if opts.ReadOnly {
		s["readOnly"] = true
	}
	return s
}
