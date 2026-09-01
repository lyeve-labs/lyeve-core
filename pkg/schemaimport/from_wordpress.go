package schemaimport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// WordPress.
//
// WordPress has no schema to export: a post type is a registration call, and
// the fields that make it useful live in Advanced Custom Fields. So the input
// here is an ACF field-group export, which is the file a WordPress project
// actually has under version control, optionally alongside the post types from
// /wp-json/wp/v2/types.
//
// A field group says which post type it belongs to in its location rules, and
// that is what names the schema. Groups that attach to something other than a
// post type, such as a taxonomy term or an options page, have no table to land
// in and are reported rather than guessed at.
//
// Every post type also gets the core WordPress columns, because a post with no
// title, body or slug is not a post. An ACF group alone would import a table of
// custom fields hanging off nothing.

type acfGroup struct {
	Key      string              `json:"key"`
	Title    string              `json:"title"`
	Fields   []acfField          `json:"fields"`
	Location [][]acfLocationRule `json:"location"`
}

type acfLocationRule struct {
	Param    string `json:"param"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

type acfField struct {
	Key          string     `json:"key"`
	Label        string     `json:"label"`
	Name         string     `json:"name"`
	Type         string     `json:"type"`
	Required     any        `json:"required"`
	DefaultValue any        `json:"default_value"`
	PostType     []string   `json:"post_type"`
	Multiple     any        `json:"multiple"`
	SubFields    []acfField `json:"sub_fields"`
	ReturnFormat any        `json:"return_format"`
	Taxonomy     any        `json:"taxonomy"`
}

// wpTypesDoc is the shape of /wp-json/wp/v2/types, used only to pick up post
// types an ACF group does not mention.
type wpTypesDoc map[string]struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// FromWordPress converts an ACF field-group export, and optionally a post-type
// listing, into a bundle.
func FromWordPress(data []byte) (*Conversion, error) {
	tree, err := decodeToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("wordpress: %w", err)
	}

	groups, types, err := readWordPressInput(tree)
	if err != nil {
		return nil, err
	}

	conv := &Conversion{Bundle: &Bundle{}}

	// Post types named by the location rules, plus any from a types listing.
	byType := map[string][]acfGroup{}
	for _, g := range groups {
		targets, unsupported := acfPostTypes(g)
		for _, u := range unsupported {
			conv.note(Identifier(g.Title), "", "field group %q skipped for %s: only groups attached to a post type have a table to import into", g.Title, u)
		}
		for _, t := range targets {
			byType[t] = append(byType[t], g)
		}
	}
	for slug := range types {
		if _, ok := byType[slug]; !ok {
			byType[slug] = nil
		}
	}
	if len(byType) == 0 {
		return nil, fmt.Errorf("wordpress: no post types found in the field groups or type listing")
	}

	names := make([]string, 0, len(byType))
	for t := range byType {
		names = append(names, t)
	}
	sort.Strings(names)

	known := map[string]string{}
	for _, t := range names {
		known[t] = Identifier(t)
	}

	for _, postType := range names {
		display := ""
		if t, ok := types[postType]; ok {
			display = t.Name
		}
		b := newBuilder(conv, postType, display)
		addWordPressCoreFields(b)

		for _, g := range byType[postType] {
			for _, f := range g.Fields {
				mapped, ok := acfFieldType(conv, b.schema.Name, f, known)
				if !ok {
					continue
				}
				mapped.Required = truthy(f.Required)
				mapped.Default = f.DefaultValue
				b.add(acfFieldName(f), mapped)
			}
		}
		conv.Bundle.Schemas = append(conv.Bundle.Schemas, b.done())
	}
	return conv.finish("wordpress")
}

// readWordPressInput accepts a bare ACF export, a types listing, or an object
// holding either or both.
func readWordPressInput(tree json.RawMessage) ([]acfGroup, wpTypesDoc, error) {
	var combined struct {
		ACF       []acfGroup `json:"acf"`
		Groups    []acfGroup `json:"groups"`
		PostTypes wpTypesDoc `json:"types"`
	}
	if err := json.Unmarshal(tree, &combined); err == nil {
		groups := combined.ACF
		if len(groups) == 0 {
			groups = combined.Groups
		}
		if len(groups) > 0 || len(combined.PostTypes) > 0 {
			return groups, combined.PostTypes, nil
		}
	}

	var groups []acfGroup
	if err := json.Unmarshal(tree, &groups); err == nil && len(groups) > 0 {
		return groups, nil, nil
	}

	var one acfGroup
	if err := json.Unmarshal(tree, &one); err == nil && len(one.Fields) > 0 {
		return []acfGroup{one}, nil, nil
	}

	var types wpTypesDoc
	if err := json.Unmarshal(tree, &types); err == nil && len(types) > 0 {
		return nil, types, nil
	}
	return nil, nil, fmt.Errorf("wordpress: expected an ACF field-group export or a post-type listing")
}

// acfPostTypes reads the location rules for the post types a group attaches to,
// and reports the rule kinds that have no table to land in.
func acfPostTypes(g acfGroup) (targets []string, unsupported []string) {
	seen := map[string]bool{}
	other := map[string]bool{}
	for _, group := range g.Location {
		for _, rule := range group {
			if rule.Param == "post_type" && (rule.Operator == "==" || rule.Operator == "") {
				if !seen[rule.Value] {
					seen[rule.Value] = true
					targets = append(targets, rule.Value)
				}
				continue
			}
			if rule.Param != "" && !other[rule.Param] {
				other[rule.Param] = true
				unsupported = append(unsupported, rule.Param)
			}
		}
	}
	sort.Strings(targets)
	sort.Strings(unsupported)
	if len(targets) > 0 {
		// The group does land somewhere, so the other rules are alternatives
		// rather than an omission worth reporting.
		return targets, nil
	}
	return nil, unsupported
}

// addWordPressCoreFields adds the columns every WordPress post has, so an
// imported type holds its content and not only its custom fields.
func addWordPressCoreFields(b *builder) {
	b.add("title", domain.SchemaField{FieldType: "text", Required: true, Indexed: true})
	b.add("slug", domain.SchemaField{FieldType: "text", Unique: true})
	b.add("content", domain.SchemaField{FieldType: "rich_text"})
	b.add("excerpt", domain.SchemaField{FieldType: "text"})
	b.add("status", domain.SchemaField{FieldType: "text", Default: "publish"})
	b.add("published_at", domain.SchemaField{FieldType: "datetime"})
}

func acfFieldName(f acfField) string {
	if f.Name != "" {
		return f.Name
	}
	if f.Label != "" {
		return f.Label
	}
	return f.Key
}

func acfFieldType(conv *Conversion, schema string, f acfField, known map[string]string) (domain.SchemaField, bool) {
	name := acfFieldName(f)
	switch f.Type {
	case "text", "select", "radio", "button_group", "password":
		if f.Type == "password" {
			conv.note(schema, name, "field dropped: passwords are not carried between systems")
			return domain.SchemaField{}, false
		}
		if f.Type != "text" {
			conv.note(schema, name, "stored as text: the allowed values are not enforced by the column")
		}
		return domain.SchemaField{FieldType: "text"}, true
	case "textarea":
		return domain.SchemaField{FieldType: "text"}, true
	case "wysiwyg":
		return domain.SchemaField{FieldType: "rich_text"}, true
	case "number", "range":
		return domain.SchemaField{FieldType: "number"}, true
	case "true_false":
		return domain.SchemaField{FieldType: "boolean"}, true
	case "email":
		return domain.SchemaField{FieldType: "email"}, true
	case "url", "page_link", "oembed":
		return domain.SchemaField{FieldType: "url"}, true
	case "image", "file":
		return domain.SchemaField{FieldType: "media"}, true
	case "gallery":
		conv.note(schema, name, "kept as json: a gallery holds several files, which one media column cannot")
		return domain.SchemaField{FieldType: "json"}, true
	case "date_picker":
		return domain.SchemaField{FieldType: "date"}, true
	case "date_time_picker":
		return domain.SchemaField{FieldType: "datetime"}, true
	case "time_picker":
		conv.note(schema, name, "stored as text: this engine has no time-only column type")
		return domain.SchemaField{FieldType: "text"}, true
	case "color_picker":
		return domain.SchemaField{FieldType: "text"}, true
	case "checkbox", "link", "google_map", "group", "repeater", "flexible_content", "clone":
		conv.note(schema, name, "kept as json: an ACF %s holds a structure with no column equivalent", f.Type)
		return domain.SchemaField{FieldType: "json"}, true
	case "message", "tab", "accordion":
		// Layout only: these render in the editor and store nothing.
		return domain.SchemaField{}, false
	case "post_object", "relationship":
		return acfRelation(conv, schema, name, f, known)
	case "taxonomy", "user":
		conv.note(schema, name, "kept as json: %s references live outside the imported post types", f.Type)
		return domain.SchemaField{FieldType: "json"}, true
	default:
		conv.note(schema, name, "kept as json: unrecognized ACF type %q", f.Type)
		return domain.SchemaField{FieldType: "json"}, true
	}
}

// acfRelation maps a post_object or relationship field. relationship always
// holds several, post_object holds one unless multiple is set.
func acfRelation(conv *Conversion, schema, name string, f acfField, known map[string]string) (domain.SchemaField, bool) {
	targets := f.PostType
	if len(targets) != 1 {
		if len(targets) == 0 {
			conv.note(schema, name, "kept as json: the field does not say which post type it points at")
		} else {
			conv.note(schema, name, "kept as json: a field accepting several post types has no single relation target")
		}
		return domain.SchemaField{FieldType: "json"}, true
	}
	to, ok := known[targets[0]]
	if !ok {
		conv.note(schema, name, "reference to %q kept as json: that post type is not being imported", targets[0])
		return domain.SchemaField{FieldType: "json"}, true
	}

	rel := domain.RelBelongsTo
	if f.Type == "relationship" || truthy(f.Multiple) {
		rel = domain.RelManyToMany
	}
	return domain.SchemaField{FieldType: "relation", RelationTo: to, RelationType: rel}, true
}

// truthy reads ACF's booleans, which come across as 0/1, "0"/"1", or true/false
// depending on how the export was written.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		s := strings.TrimSpace(strings.ToLower(t))
		return s == "1" || s == "true" || s == "yes"
	default:
		return false
	}
}
