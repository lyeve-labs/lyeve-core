package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Content types declared in the configuration tree.
//
// A deployment that keeps its settings in files usually wants its content types
// there too: the whole project then stands up from a repository, with no admin
// UI involved and no click-by-click recreation when it is deployed somewhere
// else.
//
//	schemas:
//	  - name: article
//	    display_name: Article
//	    fields:
//	      - name: title
//	        field_type: text
//	        required: true
//
// The section sits in the same files as the settings and composes the same way,
// through $include and conf.d, so content types can live in their own file:
//
//	$include: schemas/*.yaml
//
// Declarations are additive. A schema named in the tree is created or updated at
// boot. One that is only in the database is left alone, because a file that
// happens not to mention a content type is not an instruction to drop the table
// and everything in it.

// LoadDeclaredSchemas reads the schemas section of the configuration tree and
// returns it as a bundle document, or nil when the tree declares none.
//
// Returned as JSON rather than as parsed schemas so this package does not need
// to know the shape of a schema. The caller hands it to the import package,
// which owns that.
func LoadDeclaredSchemas(path string) (json.RawMessage, error) {
	if path == "" {
		path = discoverConfigPath()
		if path == "" {
			return nil, nil
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		// A missing path is reported by LoadFiles, which runs first. Staying
		// quiet here keeps one boot failure from being reported twice.
		return nil, nil
	}
	files, err := expandPath(path, info.IsDir())
	if err != nil {
		return nil, err
	}

	var all []any
	loaded := map[string]bool{}
	for _, f := range files {
		err := collectSection(f, schemasKey, loaded, 0, func(val *yaml.Node, file string) error {
			if val.Kind != yaml.SequenceNode {
				return fmt.Errorf("config %s:%d: schemas must be a list of content types", file, val.Line)
			}
			var decoded []any
			if err := val.Decode(&decoded); err != nil {
				return fmt.Errorf("config %s:%d: %w", file, val.Line, err)
			}
			all = append(all, decoded...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(all) == 0 {
		return nil, nil
	}

	doc, err := json.Marshal(map[string]any{"schemas": all})
	if err != nil {
		return nil, fmt.Errorf("read declared schemas: %w", err)
	}
	return doc, nil
}

// collectSection hands visit the value of every top-level key section in path
// and the files it includes, following includes the same way the settings do.
// loaded is shared across the whole tree so a file included twice is read
// once.
func collectSection(path, key string, loaded map[string]bool, depth int, visit func(val *yaml.Node, file string) error) error {
	if depth > maxIncludeDepth {
		return fmt.Errorf("config %s: includes nested more than %d deep", path, maxIncludeDepth)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if loaded[abs] {
		return nil
	}
	loaded[abs] = true

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}

	includes, err := includePaths(root, path)
	if err != nil {
		return err
	}
	for _, inc := range includes {
		if err := collectSection(inc, key, loaded, depth+1, visit); err != nil {
			return err
		}
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != key {
			continue
		}
		if err := visit(root.Content[i+1], path); err != nil {
			return err
		}
	}
	return nil
}
