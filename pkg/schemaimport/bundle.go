// Package schemaimport moves content-type definitions between projects.
//
// A bundle is the portable form of a set of schemas: export one from an
// instance, keep it in version control, and import it somewhere else. The same
// representation is what a deployment writes when it declares its content types
// in files rather than creating them through the admin UI, so a project can be
// stood up from a repository with no admin involved at all.
//
// Definitions from other systems reach the engine through the same path.
// A converter turns a foreign description into schemas, and from there import
// is identical whether the definitions came from a LyEve export, a JSON Schema
// document, or another CMS.
package schemaimport

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// BundleVersion is the format version this package writes. A bundle carrying a
// later version is refused rather than read on a best-effort basis, because a
// field this build does not understand would be dropped silently and the import
// would look like it worked.
const BundleVersion = 1

// Bundle is a portable set of content-type definitions.
type Bundle struct {
	Version int             `json:"version" yaml:"version"`
	Schemas []domain.Schema `json:"schemas" yaml:"schemas"`

	// Source records where the definitions came from, for a human reading the
	// file later. It has no effect on import.
	Source string `json:"source,omitempty" yaml:"source,omitempty"`
}

// ParseBundle reads a bundle from YAML or JSON.
//
// Both are accepted through the YAML parser, which handles JSON as a subset.
// The decoded tree is then re-encoded and read through the schema types' JSON
// tags, so a bundle and an API payload describe a schema with exactly the same
// key names. Keeping a second set of yaml tags in step with them is a
// reliable source of drift.
func ParseBundle(data []byte) (*Bundle, error) {
	var tree any
	if err := yaml.Unmarshal(data, &tree); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	if tree == nil {
		return nil, fmt.Errorf("parse bundle: no content")
	}

	encoded, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}

	var b Bundle
	dec := json.NewDecoder(strings.NewReader(string(encoded)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		// A misspelled key is reported rather than ignored: a bundle whose
		// "fields" is spelled "field" would import as a table with no columns.
		return nil, fmt.Errorf("parse bundle: %w", err)
	}

	if b.Version == 0 {
		b.Version = BundleVersion
	}
	if b.Version > BundleVersion {
		return nil, fmt.Errorf("bundle version %d is newer than this engine understands (%d)", b.Version, BundleVersion)
	}
	if len(b.Schemas) == 0 {
		return nil, fmt.Errorf("bundle declares no schemas")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}

// Validate checks every schema in the bundle against the same rules the schema
// API applies, so a bad definition is reported against its file rather than
// part-way through an import that has already created tables.
func (b *Bundle) Validate() error {
	seen := make(map[string]struct{}, len(b.Schemas))
	for i := range b.Schemas {
		s := &b.Schemas[i]
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("schema %q is declared twice", s.Name)
		}
		seen[s.Name] = struct{}{}
		if err := domain.ValidateSchema(s); err != nil {
			return fmt.Errorf("schema %q: %w", s.Name, err)
		}
	}
	return nil
}

// MarshalYAML renders the bundle as YAML for export.
func (b *Bundle) MarshalYAML() ([]byte, error) {
	// Through JSON first, for the same reason ParseBundle reads through it:
	// the JSON tags are the single description of a schema's shape.
	encoded, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("render bundle: %w", err)
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		return nil, fmt.Errorf("render bundle: %w", err)
	}
	out, err := yaml.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("render bundle: %w", err)
	}
	return out, nil
}

// Dependencies returns the schema names a definition needs to exist before it
// can be applied.
//
// Only belongs_to and many_to_many carry a constraint on this table: belongs_to
// emits a foreign key against the referenced table, and many_to_many builds a
// pivot referencing both. has_one and has_many hold no column here, so they
// impose no ordering.
func Dependencies(s domain.Schema) []string {
	var deps []string
	seen := map[string]struct{}{}
	for _, f := range s.Fields {
		if f.FieldType != "relation" || f.RelationTo == "" || f.RelationTo == s.Name {
			continue
		}
		switch f.RelationType {
		case domain.RelBelongsTo, domain.RelManyToMany:
		default:
			continue
		}
		if _, dup := seen[f.RelationTo]; dup {
			continue
		}
		seen[f.RelationTo] = struct{}{}
		deps = append(deps, f.RelationTo)
	}
	sort.Strings(deps)
	return deps
}

// FirstPass returns the definition a schema in a reference cycle is applied
// with the first time: every relation to another schema left out.
//
// A relation becomes a foreign key, or a pivot table with two of them, and
// either names a table that has to exist already. In a cycle one of the two
// tables is always created first, so its full definition fails on the table
// the other side has yet to create, and the import stops before the second
// pass runs. A relation to the schema itself is kept, because its table is the
// one being created. The second pass applies the full definition once every
// table exists, which adds the relations as a change to an existing table.
func FirstPass(s domain.Schema) domain.Schema {
	out := s
	out.Fields = make([]domain.SchemaField, 0, len(s.Fields))
	for _, f := range s.Fields {
		if f.FieldType == "relation" && f.RelationTo != "" && f.RelationTo != s.Name {
			continue
		}
		out.Fields = append(out.Fields, f)
	}
	return out
}

// Ordered returns the bundle's schemas arranged so that every schema follows
// the ones it references.
//
// Applying them in the order they happen to appear in a file fails as soon as a
// belongs_to points at a table that does not exist yet, which is the normal
// case for anything exported from a real project.
//
// Schemas caught in a reference cycle cannot be fully applied in one pass, so
// they are emitted last in name order and named in the second return. The
// importer applies those a second time once every table exists, which adds the
// constraints the first pass had to leave out.
func (b *Bundle) Ordered() (ordered []domain.Schema, cyclic []string) {
	byName := make(map[string]domain.Schema, len(b.Schemas))
	names := make([]string, 0, len(b.Schemas))
	for _, s := range b.Schemas {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	sort.Strings(names)

	const (
		unvisited = 0
		active    = 1
		done      = 2
	)
	state := make(map[string]int, len(names))
	inCycle := map[string]struct{}{}
	// path is the chain of schemas being expanded, the outermost first.
	var path []string

	var visit func(name string)
	visit = func(name string) {
		switch state[name] {
		case unvisited:
			// Expanded below.
		case active:
			// Reached a schema still being expanded, so this edge closes a
			// cycle. Every schema on the path from that one to here is in
			// it, not only the one the edge reached: each references the
			// next, so whichever is applied first names a table that does
			// not exist yet. Record them and unwind rather than recursing
			// forever.
			for i := len(path) - 1; i >= 0; i-- {
				inCycle[path[i]] = struct{}{}
				if path[i] == name {
					break
				}
			}
			return
		case done:
			return
		}
		state[name] = active
		path = append(path, name)
		defer func() { path = path[:len(path)-1] }()
		s, known := byName[name]
		if known {
			for _, dep := range Dependencies(s) {
				// A reference to something outside the bundle is left to the
				// engine: it may already exist in the target, and refusing here
				// would block importing one part of a project at a time.
				if _, ok := byName[dep]; ok {
					visit(dep)
				}
			}
		}
		state[name] = done
		if known {
			ordered = append(ordered, s)
		}
	}

	for _, name := range names {
		visit(name)
	}

	for name := range inCycle {
		cyclic = append(cyclic, name)
	}
	sort.Strings(cyclic)
	return ordered, cyclic
}
