package core

import "sort"

// FeatureDeclarer is a plugin that gates on names other than its own, such as
// a capability licensed separately from the plugin. The engine hands every declared name
// to the licensing implementation, and a build that links none grants every
// one of them. A plugin that gates on a name it does not declare stays locked
// on such a build.
type FeatureDeclarer interface {
	DeclaredFeatures() []string
}

// CompiledFeatureNames is the name of every compiled plugin and every name a
// compiled plugin declares through FeatureDeclarer, sorted and without
// repeats. It is empty until the plugin registry is wired.
func CompiledFeatureNames() []string {
	out := []string{}
	if RegisteredPlugins == nil {
		return out
	}
	seen := map[string]struct{}{}
	add := func(name string) {
		if _, dup := seen[name]; dup || name == "" {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, name := range RegisteredPlugins() {
		add(name)
		if LookupPlugin == nil {
			continue
		}
		factory := LookupPlugin(name)
		if factory == nil {
			continue
		}
		if d, ok := factory().(FeatureDeclarer); ok {
			for _, declared := range d.DeclaredFeatures() {
				add(declared)
			}
		}
	}
	sort.Strings(out)
	return out
}
