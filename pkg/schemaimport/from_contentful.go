package schemaimport

import (
	"encoding/json"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Contentful content types.
//
// Accepts a space export, which nests content types under a "contentTypes" key,
// a bare list of content types, or a single one.
//
// Two Contentful ideas have no column equivalent. A Link is either to an Entry,
// which is a relation, or to an Asset, which is media. The distinction is in
// linkType rather than in type. And an Array is a list of either, so its items
// have to be read to tell a many-to-many relation from a list of files.

type contentfulExport struct {
	ContentTypes []contentfulType `json:"contentTypes"`
}

type contentfulType struct {
	Sys struct {
		ID string `json:"id"`
	} `json:"sys"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	DisplayField string            `json:"displayField"`
	Fields       []contentfulField `json:"fields"`
}

type contentfulField struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Type        string                 `json:"type"`
	LinkType    string                 `json:"linkType"`
	Required    bool                   `json:"required"`
	Localized   bool                   `json:"localized"`
	Disabled    bool                   `json:"disabled"`
	Omitted     bool                   `json:"omitted"`
	Validations []contentfulValidation `json:"validations"`
	Items       *contentfulField       `json:"items"`
}

type contentfulValidation struct {
	LinkContentType []string `json:"linkContentType"`
	Unique          *bool    `json:"unique"`
}

// FromContentful converts Contentful content types into a bundle.
func FromContentful(data []byte) (*Conversion, error) {
	tree, err := decodeToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("contentful: %w", err)
	}

	var types []contentfulType
	var export contentfulExport
	switch {
	case json.Unmarshal(tree, &export) == nil && len(export.ContentTypes) > 0:
		types = export.ContentTypes
	case json.Unmarshal(tree, &types) == nil && len(types) > 0:
	default:
		var one contentfulType
		if err := json.Unmarshal(tree, &one); err != nil || len(one.Fields) == 0 {
			return nil, fmt.Errorf("contentful: expected a space export, a list of content types, or one content type")
		}
		types = []contentfulType{one}
	}

	conv := &Conversion{Bundle: &Bundle{}}
	known := map[string]string{}
	for _, t := range types {
		if name := contentfulName(t); name != "" {
			known[name] = Identifier(name)
		}
	}

	for _, t := range types {
		name := contentfulName(t)
		if name == "" {
			continue
		}
		b := newBuilder(conv, name, t.Name)
		localized := false

		for _, f := range t.Fields {
			if f.Omitted {
				continue
			}
			if f.Localized {
				localized = true
			}
			mapped, ok := contentfulFieldType(conv, b.schema.Name, f, known)
			if !ok {
				continue
			}
			mapped.Required = f.Required
			for _, v := range f.Validations {
				if v.Unique != nil && *v.Unique {
					mapped.Unique = true
				}
			}
			if f.Localized {
				// The mark is carried where this engine accepts it and named
				// where it does not, because the schema validator refuses the
				// whole definition over one field it cannot translate.
				mapped.Localized = true
				if why := core.LocalizedRefusal(mapped); why != "" {
					mapped.Localized = false
					conv.note(b.schema.Name, fieldIdent(f), "localized in Contentful, imported unlocalized: %s", why)
				}
			}
			b.add(fieldIdent(f), mapped)
		}

		if localized {
			// Contentful stores every locale's value inside the field. This
			// engine keeps one row per locale, so the values have to be split
			// when the content is moved, not when the schema is.
			b.schema.WithLocalization = true
			conv.note(b.schema.Name, "", "localized fields: this engine stores one row per locale, so each locale's values import as separate entries")
		}
		if len(b.schema.Fields) == 0 {
			conv.note(b.schema.Name, "", "content type skipped: no fields could be mapped")
			continue
		}
		conv.Bundle.Schemas = append(conv.Bundle.Schemas, b.done())
	}
	return conv.finish("contentful")
}

func contentfulName(t contentfulType) string {
	if t.Sys.ID != "" {
		return t.Sys.ID
	}
	return t.Name
}

func fieldIdent(f contentfulField) string {
	if f.ID != "" {
		return f.ID
	}
	return f.Name
}

func contentfulFieldType(conv *Conversion, schema string, f contentfulField, known map[string]string) (domain.SchemaField, bool) {
	name := fieldIdent(f)
	switch f.Type {
	case "Symbol":
		return domain.SchemaField{FieldType: "text"}, true
	case "Text":
		return domain.SchemaField{FieldType: "rich_text"}, true
	case "RichText":
		return domain.SchemaField{FieldType: "json"}, true
	case "Integer", "Number":
		return domain.SchemaField{FieldType: "number"}, true
	case "Boolean":
		return domain.SchemaField{FieldType: "boolean"}, true
	case "Date":
		return domain.SchemaField{FieldType: "datetime"}, true
	case "Object":
		return domain.SchemaField{FieldType: "json"}, true
	case "Location":
		conv.note(schema, name, "kept as json: a latitude and longitude pair has no column type")
		return domain.SchemaField{FieldType: "json"}, true

	case "Link":
		return contentfulLink(conv, schema, name, f, known, domain.RelBelongsTo)

	case "Array":
		if f.Items == nil {
			conv.note(schema, name, "kept as json: the list does not say what it holds")
			return domain.SchemaField{FieldType: "json"}, true
		}
		if f.Items.Type == "Link" {
			return contentfulLink(conv, schema, name, *f.Items, known, domain.RelManyToMany)
		}
		conv.note(schema, name, "kept as json: a list of values has no column type")
		return domain.SchemaField{FieldType: "json"}, true

	default:
		conv.note(schema, name, "kept as json: unrecognized Contentful type %q", f.Type)
		return domain.SchemaField{FieldType: "json"}, true
	}
}

// contentfulLink maps a Link, which is a relation to an entry or a reference to
// an uploaded file depending on linkType.
func contentfulLink(conv *Conversion, schema, name string, f contentfulField, known map[string]string, rel string) (domain.SchemaField, bool) {
	if f.LinkType == "Asset" {
		return domain.SchemaField{FieldType: "media"}, true
	}

	targets := linkTargets(f)
	switch len(targets) {
	case 0:
		conv.note(schema, name, "kept as json: the link does not say which content type it points at")
		return domain.SchemaField{FieldType: "json"}, true
	case 1:
	default:
		conv.note(schema, name, "kept as json: a link accepting several content types has no single relation target")
		return domain.SchemaField{FieldType: "json"}, true
	}

	to, ok := known[targets[0]]
	if !ok {
		conv.note(schema, name, "link to %q kept as json: that content type is not being imported", targets[0])
		return domain.SchemaField{FieldType: "json"}, true
	}
	return domain.SchemaField{FieldType: "relation", RelationTo: to, RelationType: rel}, true
}

func linkTargets(f contentfulField) []string {
	for _, v := range f.Validations {
		if len(v.LinkContentType) > 0 {
			return v.LinkContentType
		}
	}
	return nil
}
