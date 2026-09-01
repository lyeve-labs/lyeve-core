package schemaimport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// JSON Schema and OpenAPI.
//
// A JSON Schema object describes one content type: its properties become
// fields, and the required list marks which of them are mandatory. OpenAPI adds
// nothing new here beyond where the documents live, so a component schema is
// read through the same mapper.
//
// The type mapping is lossy in one direction and exact in the other: every
// engine field type has a JSON Schema representation, but JSON Schema can
// describe shapes with no column equivalent. Those become json columns, which
// preserves the data, and each one is reported so nobody assumes a nested
// object became a related table.

// jsonSchemaDoc is the subset of JSON Schema this converter reads.
type jsonSchemaDoc struct {
	Title       string                   `json:"title"`
	Description string                   `json:"description"`
	Type        any                      `json:"type"`
	Properties  map[string]jsonSchemaDoc `json:"properties"`
	Required    []string                 `json:"required"`
	Format      string                   `json:"format"`
	Ref         string                   `json:"$ref"`
	Enum        []any                    `json:"enum"`
	Items       *jsonSchemaDoc           `json:"items"`
	MaxLength   *int                     `json:"maxLength"`
	Default     any                      `json:"default"`

	ContentMediaType string `json:"contentMediaType"`

	// Composition keywords. Only the "nullable via anyOf" shape is understood.
	// Anything else is reported rather than guessed at.
	AnyOf []jsonSchemaDoc `json:"anyOf"`
	OneOf []jsonSchemaDoc `json:"oneOf"`
	AllOf []jsonSchemaDoc `json:"allOf"`
}

// FromJSONSchema converts one or more JSON Schema documents into a bundle.
//
// Accepts a single object schema, or a map of named schemas as an OpenAPI
// components block does. YAML and JSON are both read.
func FromJSONSchema(data []byte) (*Conversion, error) {
	tree, err := decodeToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("json schema: %w", err)
	}

	docs, err := namedSchemaDocs(tree)
	if err != nil {
		return nil, err
	}
	conv := &Conversion{Bundle: &Bundle{}}
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)

	known := map[string]string{}
	for _, name := range names {
		known[name] = Identifier(name)
	}

	for _, name := range names {
		s, ok := convertJSONSchemaDoc(conv, name, docs[name], known)
		if ok {
			conv.Bundle.Schemas = append(conv.Bundle.Schemas, s)
		}
	}
	return conv.finish("json-schema")
}

// FromOpenAPI converts the component schemas of an OpenAPI document.
func FromOpenAPI(data []byte) (*Conversion, error) {
	tree, err := decodeToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]jsonSchemaDoc `json:"schemas"`
		} `json:"components"`
		// Swagger 2 kept them at the top level.
		Definitions map[string]jsonSchemaDoc `json:"definitions"`
	}
	if err := json.Unmarshal(tree, &doc); err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}

	docs := doc.Components.Schemas
	if len(docs) == 0 {
		docs = doc.Definitions
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("openapi: document declares no component schemas")
	}

	conv := &Conversion{Bundle: &Bundle{}}
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)

	known := map[string]string{}
	for _, name := range names {
		known[name] = Identifier(name)
	}
	for _, name := range names {
		s, ok := convertJSONSchemaDoc(conv, name, docs[name], known)
		if ok {
			conv.Bundle.Schemas = append(conv.Bundle.Schemas, s)
		}
	}
	return conv.finish("openapi")
}

// namedSchemaDocs reads either a single schema or a map of them.
func namedSchemaDocs(tree json.RawMessage) (map[string]jsonSchemaDoc, error) {
	var single jsonSchemaDoc
	if err := json.Unmarshal(tree, &single); err == nil && single.Properties != nil {
		name := single.Title
		if name == "" {
			name = "imported"
		}
		return map[string]jsonSchemaDoc{name: single}, nil
	}

	var many map[string]jsonSchemaDoc
	if err := json.Unmarshal(tree, &many); err != nil {
		return nil, fmt.Errorf("json schema: expected an object schema or a map of named schemas: %w", err)
	}
	out := map[string]jsonSchemaDoc{}
	for name, doc := range many {
		if doc.Properties == nil {
			continue
		}
		out[name] = doc
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("json schema: no object schemas with properties found")
	}
	return out, nil
}

func convertJSONSchemaDoc(conv *Conversion, name string, doc jsonSchemaDoc, known map[string]string) (domain.Schema, bool) {
	display := doc.Title
	b := newBuilder(conv, name, display)

	required := map[string]bool{}
	for _, r := range doc.Required {
		required[r] = true
	}

	props := make([]string, 0, len(doc.Properties))
	for p := range doc.Properties {
		props = append(props, p)
	}
	sort.Strings(props)

	for _, prop := range props {
		field, ok := jsonSchemaField(conv, b.schema.Name, prop, doc.Properties[prop], known)
		if !ok {
			continue
		}
		field.Required = required[prop]
		b.add(prop, field)
	}

	if len(b.schema.Fields) == 0 {
		conv.note(b.schema.Name, "", "content type skipped: no properties could be mapped to fields")
		return domain.Schema{}, false
	}
	return b.done(), true
}

// jsonSchemaField maps one property to a field.
func jsonSchemaField(conv *Conversion, schema, prop string, p jsonSchemaDoc, known map[string]string) (domain.SchemaField, bool) {
	// A reference to another named schema becomes a relation, which is the
	// whole reason to carry a reference across rather than inlining it.
	if target := refTarget(p.Ref); target != "" {
		if to, ok := known[target]; ok {
			return domain.SchemaField{
				FieldType:    "relation",
				RelationTo:   to,
				RelationType: domain.RelBelongsTo,
			}, true
		}
		conv.note(schema, prop, "reference to %q kept as json: no schema by that name is being imported", target)
		return domain.SchemaField{FieldType: "json"}, true
	}

	// The common nullable shape, {"anyOf": [{...}, {"type": "null"}]}, is the
	// same field with Required off.
	if inner, ok := unwrapNullable(p); ok {
		return jsonSchemaField(conv, schema, prop, inner, known)
	}
	if len(p.AllOf) == 1 {
		return jsonSchemaField(conv, schema, prop, p.AllOf[0], known)
	}
	if len(p.OneOf) > 0 || len(p.AnyOf) > 0 || len(p.AllOf) > 1 {
		conv.note(schema, prop, "kept as json: a combined schema has no single column type")
		return domain.SchemaField{FieldType: "json"}, true
	}

	switch primaryType(p.Type) {
	case "string":
		return domain.SchemaField{FieldType: stringFieldType(p), Default: p.Default}, true
	case "integer", "number":
		return domain.SchemaField{FieldType: "number", Default: p.Default}, true
	case "boolean":
		return domain.SchemaField{FieldType: "boolean", Default: p.Default}, true
	case "array":
		if p.Items != nil {
			if target := refTarget(p.Items.Ref); target != "" {
				if to, ok := known[target]; ok {
					return domain.SchemaField{
						FieldType:    "relation",
						RelationTo:   to,
						RelationType: domain.RelManyToMany,
					}, true
				}
			}
		}
		conv.note(schema, prop, "kept as json: a list of values has no column type")
		return domain.SchemaField{FieldType: "json"}, true
	case "object":
		conv.note(schema, prop, "kept as json: a nested object is stored whole, not as a related table")
		return domain.SchemaField{FieldType: "json"}, true
	case "null":
		conv.note(schema, prop, "field dropped: its only type is null")
		return domain.SchemaField{}, false
	case "":
		// No type at all describes any value.
		conv.note(schema, prop, "kept as json: the property declares no type")
		return domain.SchemaField{FieldType: "json"}, true
	default:
		conv.note(schema, prop, "kept as json: unrecognized type %q", primaryType(p.Type))
		return domain.SchemaField{FieldType: "json"}, true
	}
}

// stringFieldType picks the narrowest field type a string property fits.
func stringFieldType(p jsonSchemaDoc) string {
	switch strings.ToLower(p.Format) {
	case "email", "idn-email":
		return "email"
	case "uri", "url", "iri":
		return "url"
	case "date":
		return "date"
	case "date-time":
		return "datetime"
	case "uuid":
		return "uid"
	}
	if strings.HasPrefix(strings.ToLower(p.ContentMediaType), "image/") {
		return "media"
	}
	// text and rich_text store identically, so this only picks which editor the
	// admin offers. An explicit bound in the thousands is the one strong signal
	// that a property holds prose. Anything else defaults to text, because
	// putting a rich text editor on a slug is worse than putting a plain one on
	// a body.
	if p.MaxLength != nil && *p.MaxLength > 1000 {
		return "rich_text"
	}
	return "text"
}

// refTarget extracts the component name from a local $ref.
func refTarget(ref string) string {
	if ref == "" {
		return ""
	}
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// unwrapNullable recognizes {"anyOf":[X, {"type":"null"}]} and returns X.
func unwrapNullable(p jsonSchemaDoc) (jsonSchemaDoc, bool) {
	branches := p.AnyOf
	if len(branches) == 0 {
		branches = p.OneOf
	}
	if len(branches) != 2 {
		return jsonSchemaDoc{}, false
	}
	for i, b := range branches {
		if primaryType(b.Type) == "null" {
			return branches[1-i], true
		}
	}
	return jsonSchemaDoc{}, false
}

// primaryType reads the type keyword, which may be a string or a list. For a
// list, the first non-null entry is the real type and null only says the value
// is optional.
func primaryType(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if ok && s != "null" {
				return s
			}
		}
		if len(v) > 0 {
			return "null"
		}
	}
	return ""
}

// decodeToJSON reads YAML or JSON into canonical JSON bytes.
func decodeToJSON(data []byte) (json.RawMessage, error) {
	var tree any
	if err := yaml.Unmarshal(data, &tree); err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, fmt.Errorf("no content")
	}
	return json.Marshal(tree)
}
