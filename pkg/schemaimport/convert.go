package schemaimport

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// Converting definitions written for another system.
//
// Every converter produces a Bundle and reports what it could not carry over,
// rather than dropping it. An import that silently loses a field looks like it
// worked and is found later, by whoever notices the data is not there.
//
// Names are the first problem. Other systems allow identifiers this engine
// cannot put in DDL: hyphens, spaces, leading digits, camelCase, reserved
// words. They are folded to snake_case here, and the mapping is reported so a
// caller can see that "publishedAt" became "published_at" before any content is
// moved against the source name.

// Note records something a conversion changed or could not carry over.
type Note struct {
	Schema  string `json:"schema"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// Conversion is a converter's output: the schemas it produced, and everything a
// human needs to check before importing them.
type Conversion struct {
	Bundle *Bundle `json:"bundle"`

	// Renamed maps an original identifier to the one used here, as
	// "publishedAt" -> "published_at". Keyed "<schema>" or "<schema>.<field>".
	Renamed map[string]string `json:"renamed,omitempty"`

	// Notes records approximations and omissions.
	Notes []Note `json:"notes,omitempty"`
}

func (c *Conversion) note(schema, field, format string, args ...any) {
	c.Notes = append(c.Notes, Note{Schema: schema, Field: field, Message: fmt.Sprintf(format, args...)})
}

func (c *Conversion) rename(key, from, to string) {
	if from == to {
		return
	}
	if c.Renamed == nil {
		c.Renamed = map[string]string{}
	}
	c.Renamed[key] = from + " -> " + to
}

// finish orders the bundle and sorts the notes so a conversion of the same
// input always reads the same way.
func (c *Conversion) finish(source string) (*Conversion, error) {
	if c.Bundle == nil || len(c.Bundle.Schemas) == 0 {
		return nil, fmt.Errorf("%s: no content types found", source)
	}
	c.Bundle.Version = BundleVersion
	c.Bundle.Source = source

	sort.Slice(c.Bundle.Schemas, func(i, j int) bool {
		return c.Bundle.Schemas[i].Name < c.Bundle.Schemas[j].Name
	})
	sort.Slice(c.Notes, func(i, j int) bool {
		if c.Notes[i].Schema != c.Notes[j].Schema {
			return c.Notes[i].Schema < c.Notes[j].Schema
		}
		if c.Notes[i].Field != c.Notes[j].Field {
			return c.Notes[i].Field < c.Notes[j].Field
		}
		return c.Notes[i].Message < c.Notes[j].Message
	})

	if err := c.Bundle.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return c, nil
}

var nonIdentRe = regexp.MustCompile(`[^a-z0-9_]+`)

// reservedNames are column names the engine manages. A source field landing on
// one is suffixed rather than colliding with the engine's own column.
var reservedNames = map[string]bool{
	"id": true, "created_at": true, "updated_at": true,
	"deleted_at": true, "_status": true, "_locale": true,
}

// Identifier folds a foreign name into one this engine can put in DDL.
//
// camelCase and PascalCase split on the case boundary, so publishedAt becomes
// published_at rather than publishedat: the second is legal and unreadable, and
// an operator matching it against the source system would not recognize it.
func Identifier(name string) string {
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			prevLower := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if i > 0 && (prevLower || nextLower) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}

	out := nonIdentRe.ReplaceAllString(b.String(), "_")
	out = strings.Trim(out, "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if out == "" {
		return ""
	}
	// An identifier must not start with a digit, and 63 characters is the
	// limit the engine enforces.
	if out[0] >= '0' && out[0] <= '9' {
		out = "f_" + out
	}
	if len(out) > 63 {
		out = out[:63]
		out = strings.TrimRight(out, "_")
	}
	return out
}

// fieldName folds a field name and keeps it clear of the engine's own columns
// and of names already taken in the same schema.
func fieldName(raw string, taken map[string]bool) string {
	name := Identifier(raw)
	if name == "" {
		return ""
	}
	if reservedNames[name] {
		name += "_field"
	}
	if !taken[name] {
		return name
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", name, i)
		if !taken[candidate] {
			return candidate
		}
	}
}

// displayName renders a human label for a schema when the source has none.
func displayName(raw string) string {
	parts := strings.FieldsFunc(Identifier(raw), func(r rune) bool { return r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// builder accumulates one schema's fields while keeping names unique.
type builder struct {
	schema domain.Schema
	taken  map[string]bool
	conv   *Conversion
}

func newBuilder(conv *Conversion, rawName, display string) *builder {
	name := Identifier(rawName)
	if display == "" {
		display = displayName(rawName)
	}
	conv.rename(name, rawName, name)
	return &builder{
		schema: domain.Schema{
			Name:          name,
			DisplayName:   display,
			WithCreatedAt: true,
			WithUpdatedAt: true,
		},
		taken: map[string]bool{},
		conv:  conv,
	}
}

// add appends a field, folding its name and reporting the change. Returns the
// name used, or "" when the source name could not be folded into anything
// usable.
func (b *builder) add(rawName string, f domain.SchemaField) string {
	name := fieldName(rawName, b.taken)
	if name == "" {
		b.conv.note(b.schema.Name, rawName, "field dropped: the name has no characters usable in an identifier")
		return ""
	}
	b.taken[name] = true
	b.conv.rename(b.schema.Name+"."+name, rawName, name)
	f.Name = name
	b.schema.Fields = append(b.schema.Fields, f)
	return name
}

func (b *builder) done() domain.Schema { return b.schema }
