package schemaimport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// Strapi content types.
//
// Strapi writes one schema.json per content type, under
// src/api/<name>/content-types/<name>/schema.json. Either a single file or a
// list of them is accepted, so a whole project can be converted in one pass.
//
// Relations are the part worth care. Strapi names its target as a UID,
// "api::author.author", and its cardinality with names that do not line up with
// this engine's: manyToOne holds the key on this side and is belongs_to here,
// while oneToMany holds nothing on this side and is has_many.

type strapiSchema struct {
	Kind           string `json:"kind"`
	CollectionName string `json:"collectionName"`
	Info           struct {
		SingularName string `json:"singularName"`
		PluralName   string `json:"pluralName"`
		DisplayName  string `json:"displayName"`
	} `json:"info"`
	Attributes map[string]strapiAttribute `json:"attributes"`
}

type strapiAttribute struct {
	Type       string `json:"type"`
	Required   bool   `json:"required"`
	Unique     bool   `json:"unique"`
	Default    any    `json:"default"`
	Target     string `json:"target"`
	Relation   string `json:"relation"`
	TargetAttr string `json:"targetAttribute"`
	Component  string `json:"component"`
	Repeatable bool   `json:"repeatable"`
	MaxLength  *int   `json:"maxLength"`
}

// FromStrapi converts one or more Strapi content-type schemas into a bundle.
func FromStrapi(data []byte) (*Conversion, error) {
	tree, err := decodeToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("strapi: %w", err)
	}

	var many []strapiSchema
	if err := json.Unmarshal(tree, &many); err != nil {
		var one strapiSchema
		if err := json.Unmarshal(tree, &one); err != nil {
			return nil, fmt.Errorf("strapi: expected a content-type schema or a list of them: %w", err)
		}
		many = []strapiSchema{one}
	}

	conv := &Conversion{Bundle: &Bundle{}}
	known := map[string]string{}
	for _, s := range many {
		if name := strapiName(s); name != "" {
			known[name] = Identifier(name)
		}
	}

	for _, src := range many {
		name := strapiName(src)
		if name == "" {
			continue
		}
		// A single type holds one entry rather than a collection, but the table
		// is the same shape. The difference is a rule the admin enforces, not a
		// column.
		if src.Kind == "singleType" {
			conv.note(Identifier(name), "", "imported as a collection: this engine has no single-type constraint, so limit it to one entry in the admin")
		}

		b := newBuilder(conv, name, src.Info.DisplayName)
		attrs := make([]string, 0, len(src.Attributes))
		for a := range src.Attributes {
			attrs = append(attrs, a)
		}
		sort.Strings(attrs)

		for _, attr := range attrs {
			f, ok := strapiField(conv, b.schema.Name, attr, src.Attributes[attr], known)
			if !ok {
				continue
			}
			b.add(attr, f)
		}
		if len(b.schema.Fields) == 0 {
			conv.note(b.schema.Name, "", "content type skipped: no attributes could be mapped to fields")
			continue
		}
		conv.Bundle.Schemas = append(conv.Bundle.Schemas, b.done())
	}
	return conv.finish("strapi")
}

func strapiName(s strapiSchema) string {
	for _, candidate := range []string{s.Info.SingularName, s.CollectionName, s.Info.DisplayName} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// strapiTarget reduces a Strapi UID to the content-type name:
// "api::author.author" is "author".
func strapiTarget(uid string) string {
	if uid == "" {
		return ""
	}
	if i := strings.LastIndex(uid, "."); i >= 0 {
		return uid[i+1:]
	}
	if i := strings.LastIndex(uid, "::"); i >= 0 {
		return uid[i+2:]
	}
	return uid
}

func strapiField(conv *Conversion, schema, attr string, a strapiAttribute, known map[string]string) (domain.SchemaField, bool) {
	f := domain.SchemaField{Required: a.Required, Unique: a.Unique, Default: a.Default}

	switch a.Type {
	case "string":
		f.FieldType = "text"
	case "text":
		f.FieldType = "text"
	case "richtext", "blocks":
		f.FieldType = "rich_text"
	case "email":
		f.FieldType = "email"
	case "uid":
		// Strapi's uid is a slug derived from another field, not a UUID.
		f.FieldType = "text"
		f.Unique = true
	case "password":
		conv.note(schema, attr, "field dropped: passwords are not carried between systems")
		return domain.SchemaField{}, false
	case "integer", "biginteger", "float", "decimal":
		f.FieldType = "number"
	case "boolean":
		f.FieldType = "boolean"
	case "date":
		f.FieldType = "date"
	case "datetime", "timestamp":
		f.FieldType = "datetime"
	case "time":
		// No time-only column type, and storing it as text keeps the value
		// readable rather than pinning it to an arbitrary date.
		f.FieldType = "text"
		conv.note(schema, attr, "stored as text: this engine has no time-only column type")
	case "json":
		f.FieldType = "json"
	case "enumeration":
		f.FieldType = "text"
		conv.note(schema, attr, "stored as text: the allowed values are not enforced by the column")
	case "media":
		f.FieldType = "media"
	case "component":
		f.FieldType = "json"
		conv.note(schema, attr, "component %q kept as json: nested components are stored whole, not as related tables", a.Component)
	case "dynamiczone":
		f.FieldType = "json"
		conv.note(schema, attr, "dynamic zone kept as json: its blocks have no fixed shape to map to columns")
	case "relation":
		target := strapiTarget(a.Target)
		to, ok := known[target]
		if !ok {
			f.FieldType = "json"
			conv.note(schema, attr, "relation to %q kept as json: that content type is not being imported", target)
			break
		}
		f.FieldType = "relation"
		f.RelationTo = to
		switch a.Relation {
		case "oneToOne":
			f.RelationType = domain.RelBelongsTo
		case "manyToOne":
			f.RelationType = domain.RelBelongsTo
		case "oneToMany":
			f.RelationType = domain.RelHasMany
		case "manyToMany":
			f.RelationType = domain.RelManyToMany
		default:
			f.RelationType = domain.RelBelongsTo
			conv.note(schema, attr, "relation kind %q read as belongs_to", a.Relation)
		}
	default:
		f.FieldType = "json"
		conv.note(schema, attr, "kept as json: unrecognized Strapi type %q", a.Type)
	}
	return f, true
}
