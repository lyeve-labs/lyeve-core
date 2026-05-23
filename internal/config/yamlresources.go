package config

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Resources a plugin declares in the configuration tree.
//
// An engine running with no database has no table to keep a plugin's
// resources in, so the plugin reads them from the same files as the settings:
//
//	flows:
//	  - slug: stripe-to-slack
//	    trigger: {type: trigger.webhook, config: {path: stripe}}
//	    nodes: [...]
//	webhooks:
//	  - name: orders
//	    url: https://orders.example.com/hooks
//	    secret: ${ORDERS_WEBHOOK_SECRET}
//
// Each section is a list. It composes like schemas, through $include and
// conf.d, and ${NAME} in a string value is replaced from the environment, so a
// secret can stay out of the file. The engine does not know the shape of an
// entry. The plugin that owns the section parses and validates it.

// isResourceSection reports whether key is a top-level section a plugin
// registered with core.RegisterResourceSection, which it may read with
// LoadDeclaredSection. The settings flattener leaves those sections alone,
// since an entry describes a resource rather than a setting, and a secret
// flattened into a setting would be listed by the configuration route.
func isResourceSection(key string) bool {
	return slices.Contains(core.ResourceSections(), key)
}

// LoadDeclaredSection returns every entry of one resource section of the
// configuration tree, each as a JSON document, in file order. An empty path
// searches the conventional locations. A section that is not a list stops
// the boot.
func LoadDeclaredSection(path, section string) ([]json.RawMessage, error) {
	if !isResourceSection(section) {
		return nil, fmt.Errorf("config: %q is not a resource section", section)
	}
	if path == "" {
		path = discoverConfigPath()
		if path == "" {
			return nil, nil
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		// LoadFiles reports a missing path first.
		return nil, nil
	}
	files, err := expandPath(path, info.IsDir())
	if err != nil {
		return nil, err
	}

	var out []json.RawMessage
	loaded := map[string]bool{}
	for _, f := range files {
		err := collectSection(f, section, loaded, 0, func(val *yaml.Node, file string) error {
			if val.Kind != yaml.SequenceNode {
				return fmt.Errorf("config %s:%d: %s must be a list", file, val.Line, section)
			}
			for _, item := range val.Content {
				expandScalars(item)
				var decoded any
				if err := item.Decode(&decoded); err != nil {
					return fmt.Errorf("config %s:%d: %w", file, item.Line, err)
				}
				doc, err := json.Marshal(jsonCompatible(decoded))
				if err != nil {
					return fmt.Errorf("config %s:%d: %s entry: %w", file, item.Line, section, err)
				}
				out = append(out, doc)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// expandScalars replaces ${NAME} in every string value under n.
func expandScalars(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "" || n.Tag == "!!str" {
			n.Value = expandVars(n.Value)
		}
		return
	}
	for _, c := range n.Content {
		expandScalars(c)
	}
}

// jsonCompatible turns the map[any]any a YAML decode can produce into the
// map[string]any json.Marshal accepts.
func jsonCompatible(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = jsonCompatible(e)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[fmt.Sprint(k)] = jsonCompatible(e)
		}
		return m
	case []any:
		for i, e := range t {
			t[i] = jsonCompatible(e)
		}
		return t
	default:
		return v
	}
}
