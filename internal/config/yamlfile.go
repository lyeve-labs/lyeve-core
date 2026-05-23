package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// YAML configuration tree.
//
// The engine's settings surface is well over a hundred environment variables.
// Expressed as a flat list they are unreadable and undiffable, and there is no
// way to see that the six storage_s3_* variables belong together. The file
// layer lets a deployment write them as a tree instead:
//
//	database:
//	  url: ${DATABASE_URL}
//	  max_connections: 25
//	storage:
//	  driver: s3
//	  s3:
//	    bucket: media
//	    region: eu-west-1
//	plugins:
//	  example:
//	    host: !overridable mail.example.com
//	    port: "587"
//
// Every key resolves to its environment variable name. A nested path is
// flattened by joining the segments with underscores and upper-casing, so
// storage.s3.bucket is STORAGE_S3_BUCKET, which is the variable an existing
// deployment already sets. A flat name written directly in the file works too,
// so a .env can be transcribed a line at a time without learning the tree.
//
// The plugins block is the one exception to plain flattening: its first segment
// is dropped, because a plugin key already carries its own prefix.
// plugins.example.host is EXAMPLE_HOST, not PLUGINS_EXAMPLE_HOST.

// DefaultConfigNames are the file names searched for in the working directory
// and in /etc/lyeve when no path is given.
var DefaultConfigNames = []string{"lyeve.yaml", "lyeve.yml"}

// ConfigPathEnv names the variable that points at an explicit configuration
// file or directory.
const ConfigPathEnv = "LYEVE_CONFIG"

const (
	// includeKey is the document-root key that pulls in other files. Prefixed
	// with $ so it cannot collide with a configuration section.
	includeKey = "$include"

	// overridableTag marks a scalar the admin layer is allowed to take over.
	overridableTag = "!overridable"

	// pluginsSection is the subtree whose first path segment is dropped when
	// flattening.
	pluginsSection = "PLUGINS"

	// schemasKey is the document-root section holding content-type
	// declarations. It describes tables rather than settings, so the flattener
	// leaves it alone and LoadDeclaredSchemas reads it instead.
	schemasKey = "schemas"

	// apiKeysKey is the document-root section declaring the API keys of a
	// stateless engine. Like schemas it describes resources, not settings,
	// and LoadDeclaredAPIKeys reads it.
	apiKeysKey = "api_keys"

	// maxIncludeDepth bounds include nesting. Deep enough for base plus
	// environment plus local overrides, shallow enough that a mistake is
	// reported rather than explored.
	maxIncludeDepth = 8
)

// FileValue is one setting resolved from the file tree.
type FileValue struct {
	Value string

	// Overridable is set by the !overridable tag and lets the admin layer take
	// the key over. Without it the file value is final.
	Overridable bool

	// Origin locates the value as "path/lyeve.yaml:42" for diagnostics and for
	// the admin UI's read-only fields.
	Origin string
}

// LoadFiles reads the YAML configuration tree and returns the flattened key
// layer.
//
// path may be a file or a directory. An empty path searches the default
// locations and returns an empty layer when none exists, which is what an
// engine configured entirely from the environment does. A path given
// explicitly must exist: an operator who names a file and gets silence would
// run with settings they believe are applied.
func LoadFiles(path string) (map[string]FileValue, error) {
	explicit := path != ""
	if !explicit {
		path = discoverConfigPath()
		if path == "" {
			return map[string]FileValue{}, nil
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) && explicit {
			return nil, fmt.Errorf("config path %s does not exist", path)
		}
		return nil, fmt.Errorf("config path %s: %w", path, err)
	}

	files, err := expandPath(path, info.IsDir())
	if err != nil {
		return nil, err
	}
	if len(files) == 0 && explicit {
		return nil, fmt.Errorf("config path %s contains no .yaml or .yml files", path)
	}

	out := map[string]FileValue{}
	loaded := map[string]bool{}
	for _, f := range files {
		if err := mergeFile(f, out, loaded, 0); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// discoverConfigPath returns the first configuration file found in the
// conventional locations, or "" when the deployment has none.
func discoverConfigPath() string {
	if p := strings.TrimSpace(os.Getenv(ConfigPathEnv)); p != "" {
		return p
	}
	candidates := append([]string{}, DefaultConfigNames...)
	for _, name := range DefaultConfigNames {
		candidates = append(candidates, filepath.Join("/etc/lyeve", name))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if st, err := os.Stat("/etc/lyeve/conf.d"); err == nil && st.IsDir() {
		return "/etc/lyeve/conf.d"
	}
	return ""
}

// expandPath resolves a file or directory to the ordered list of files to
// merge. Within a directory, files merge in lexical order so a 10-base.yaml,
// 20-staging.yaml naming layers predictably.
//
// A conf.d directory beside a named file is merged after it, which is the
// convention operators expect from other services and lets a package ship a
// base file that a deployment extends without editing it.
func expandPath(path string, isDir bool) ([]string, error) {
	if isDir {
		return yamlFilesIn(path)
	}
	files := []string{path}
	confD := filepath.Join(filepath.Dir(path), "conf.d")
	if st, err := os.Stat(confD); err == nil && st.IsDir() {
		extra, err := yamlFilesIn(confD)
		if err != nil {
			return nil, err
		}
		files = append(files, extra...)
	}
	return files, nil
}

func yamlFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read config directory %s: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// mergeFile parses one file into out. Includes are merged first so the
// including file's own keys win, which is what makes a base-plus-override
// layout behave the way it reads.
func mergeFile(path string, out map[string]FileValue, loaded map[string]bool, depth int) error {
	if depth > maxIncludeDepth {
		return fmt.Errorf("config %s: includes nested more than %d deep", path, maxIncludeDepth)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if loaded[abs] {
		// Already merged, by an include or by directory expansion. Re-merging
		// would be harmless for values but would let a cycle run to the depth
		// limit and report nesting when the real fault is a loop.
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
	// An empty file parses to a zero node and contributes nothing.
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config %s: top level must be a mapping of settings", path)
	}

	includes, err := includePaths(root, path)
	if err != nil {
		return err
	}
	for _, inc := range includes {
		if err := mergeFile(inc, out, loaded, depth+1); err != nil {
			return err
		}
	}

	return flattenMapping(root, nil, path, out)
}

// includePaths reads the $include entry and resolves each path relative to the
// including file. Globs are expanded and sorted so a wildcard include layers in
// the same order a directory merge would.
func includePaths(root *yaml.Node, path string) ([]string, error) {
	dir := filepath.Dir(path)
	var out []string
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != includeKey {
			continue
		}
		val := root.Content[i+1]
		var raw []string
		switch val.Kind {
		case yaml.ScalarNode:
			raw = []string{val.Value}
		case yaml.SequenceNode:
			for _, item := range val.Content {
				if item.Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("config %s:%d: $include entries must be paths", path, item.Line)
				}
				raw = append(raw, item.Value)
			}
		default:
			return nil, fmt.Errorf("config %s:%d: $include must be a path or a list of paths", path, val.Line)
		}

		for _, p := range raw {
			p = expandVars(p)
			if p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			if strings.ContainsAny(p, "*?[") {
				matches, err := filepath.Glob(p)
				if err != nil {
					return nil, fmt.Errorf("config %s:%d: bad include pattern %q: %w", path, val.Line, p, err)
				}
				sort.Strings(matches)
				out = append(out, matches...)
				continue
			}
			if _, err := os.Stat(p); err != nil {
				return nil, fmt.Errorf("config %s:%d: included file %s not found", path, val.Line, p)
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// flattenMapping walks a mapping node, joining nested keys into the flat
// variable name each setting already had.
func flattenMapping(node *yaml.Node, prefix []string, path string, out map[string]FileValue) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		if keyNode.Kind != yaml.ScalarNode {
			return fmt.Errorf("config %s:%d: setting names must be plain text", path, keyNode.Line)
		}
		key := keyNode.Value
		if key == includeKey {
			continue
		}
		// Content types, API keys and plugin resources are declarations, not
		// settings. Flattening them would turn every field into a
		// configuration key named after a column, and every key hash and
		// secret into a setting anyone can list.
		if len(prefix) == 0 && (key == schemasKey || key == apiKeysKey || isResourceSection(key)) {
			continue
		}
		segs := append(append([]string{}, prefix...), key)

		switch valNode.Kind {
		case yaml.MappingNode:
			if err := flattenMapping(valNode, segs, path, out); err != nil {
				return err
			}
		case yaml.SequenceNode:
			v, err := sequenceValue(valNode, path)
			if err != nil {
				return err
			}
			out[flatKey(segs)] = FileValue{
				Value:       v,
				Overridable: valNode.Tag == overridableTag,
				Origin:      origin(path, valNode.Line),
			}
		case yaml.ScalarNode:
			out[flatKey(segs)] = FileValue{
				Value:       scalarValue(valNode),
				Overridable: valNode.Tag == overridableTag,
				Origin:      origin(path, valNode.Line),
			}
		case yaml.AliasNode:
			if valNode.Alias == nil {
				return fmt.Errorf("config %s:%d: unresolved alias", path, valNode.Line)
			}
			return flattenMapping(&yaml.Node{
				Kind:    yaml.MappingNode,
				Content: []*yaml.Node{keyNode, valNode.Alias},
			}, prefix, path, out)
		default:
			return fmt.Errorf("config %s:%d: unsupported value for %s", path, valNode.Line, strings.Join(segs, "."))
		}
	}
	return nil
}

// flatKey renders a nested path as the variable name the engine reads. The
// plugins section drops its own segment: a plugin key already names its plugin.
func flatKey(segs []string) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		parts = append(parts, normalizeKey(s))
	}
	if len(parts) > 1 && parts[0] == pluginsSection {
		parts = parts[1:]
	}
	return strings.Join(parts, "_")
}

// scalarValue renders a YAML scalar as the text an environment variable would
// have carried, so the parsers on the other side see what they always saw.
// A null is an empty value, which is how an unset variable reads.
func scalarValue(n *yaml.Node) string {
	if n.Tag == "!!null" {
		return ""
	}
	return expandVars(n.Value)
}

// sequenceValue renders a list as the comma-separated text the engine's list
// settings are parsed from, so allowed_origins may be written as a YAML list
// without a second parser.
func sequenceValue(n *yaml.Node, path string) (string, error) {
	items := make([]string, 0, len(n.Content))
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			return "", fmt.Errorf("config %s:%d: list entries must be plain values", path, item.Line)
		}
		v := scalarValue(item)
		if strings.Contains(v, ",") {
			return "", fmt.Errorf("config %s:%d: list entry %q contains a comma, which separates entries", path, item.Line, v)
		}
		items = append(items, v)
	}
	return strings.Join(items, ","), nil
}

func origin(path string, line int) string {
	return path + ":" + strconv.Itoa(line)
}

// expandVars substitutes ${NAME} and ${NAME:-fallback} from the process
// environment, so a file can name a secret that a container injects rather than
// carrying it. An unset variable with no fallback expands to empty, matching
// what a shell would do and what the engine reads for an unset setting.
//
// $NAME without braces is left alone: values legitimately contain bare dollar
// signs, and silently eating one from a password would be worse than making the
// brace form explicit.
func expandVars(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteString(s[i:])
				break
			}
			expr := s[i+2 : i+2+end]
			b.WriteString(resolveVarExpr(expr))
			i += end + 3
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func resolveVarExpr(expr string) string {
	name, fallback, hasFallback := strings.Cut(expr, ":-")
	if v, ok := os.LookupEnv(strings.TrimSpace(name)); ok && v != "" {
		return v
	}
	if hasFallback {
		return fallback
	}
	return ""
}
