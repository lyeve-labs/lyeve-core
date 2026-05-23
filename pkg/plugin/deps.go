// Plugin dependency resolver: computes the transitive closure, topological
// ordering, and version-constraint validation for the registered plugin graph.

package plugin

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// PluginDependency describes a required peer plugin.
//
// Alias, not a copy. Plugins import pkg/core and nothing else, so they declare
// Dependencies() []core.PluginDependency. A separate struct here would make
// that a different method signature, and the Depender assertion below would
// never match any real plugin.
type PluginDependency = core.PluginDependency

// Depender is an optional interface a Plugin can implement to declare
// its required peer plugins. The activator resolves these before Start():
// missing deps are reported, version mismatches are rejected, and
// dependents are auto-loaded (if entitled) in topological order.
type Depender interface {
	core.Plugin
	Dependencies() []PluginDependency
}

// Versioned is an optional interface a Plugin can implement to declare
// its version. Used by the dependency resolver for MinVersion/MaxVersion
// constraint checking. Plugins that don't implement Versioned have a
// nil version, which satisfies any version constraint.
type Versioned interface {
	core.Plugin
	PluginVersion() string // semver, e.g., "1.2.3"
}

// DepError records a single resolution failure.
type DepError struct {
	Plugin  string `json:"plugin"`  // the plugin that declared the dep
	Dep     string `json:"dep"`     // the dependency name
	Message string `json:"message"` // human-readable explanation
}

// DepResolution is the result of dependency resolution. When HasErrors()
// returns false, Order contains the topological load sequence. When true,
// the caller should log the errors and may choose to skip the affected
// plugins (soft-fail) or abort startup (hard-fail).
type DepResolution struct {
	Order     []string   // topological load order
	Levels    [][]string // plugins grouped by BFS level for parallel start. Each level depends only on earlier levels
	AutoAdded []string   // deps loaded because another plugin needs them
	Missing   []DepError // deps not in the registry
	Version   []DepError // version constraint violations
	Cycle     []string   // plugins in a dependency cycle (first found)

	// Requires maps a dependency name to the plugins that declared it,
	// sorted. The activator names the affected dependers when it skips a
	// dependency the license does not cover, so the log says which plugin
	// is about to run without its tables rather than just which one is off.
	Requires map[string][]string
}

// HasErrors reports whether the resolution has any hard errors.
func (r *DepResolution) HasErrors() bool {
	return len(r.Missing) > 0 || len(r.Version) > 0 || len(r.Cycle) > 0
}

// pluginInfo holds the metadata extracted from a registered plugin.
type pluginInfo struct {
	name    string
	version string             // from Versioned, "" if not implemented
	depends []PluginDependency // from Depender, nil if not implemented
}

// ResolvePluginDeps computes the full transitive closure of requested
// plugins against the current registry, topologically sorts them, and
// validates version constraints.
//
// The returned DepResolution.Order includes requested + auto-added deps
// in topological order. Auto-added deps are plugins that weren't in
// requested but are required by one or more requested plugins.
func ResolvePluginDeps(requested []string) *DepResolution {
	if len(requested) == 0 {
		return &DepResolution{}
	}

	// 1. Build info for every registered plugin to traverse
	//    transitive deps without repeated factory calls.
	all := RegisteredPlugins()
	infos := make(map[string]*pluginInfo, len(all))
	for _, name := range all {
		f := LookupPlugin(name)
		if f == nil {
			continue // shouldn't happen, but defensive
		}
		p := f()
		info := &pluginInfo{name: name}
		if v, ok := p.(Versioned); ok {
			if ver := strings.TrimSpace(v.PluginVersion()); ver != "" {
				info.version = ver
			}
		}
		if d, ok := p.(Depender); ok {
			deps := d.Dependencies()
			if len(deps) > 0 {
				info.depends = make([]PluginDependency, len(deps))
				copy(info.depends, deps)
			}
		}
		infos[name] = info
	}

	// 2. Compute transitive closure: start with requested, walk deps.
	closure := make(map[string]bool, len(requested))
	queue := make([]string, len(requested))
	copy(queue, requested)

	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]

		if closure[name] {
			continue
		}
		closure[name] = true

		info := infos[name]
		if info == nil {
			continue // not registered: the activator catches this in validation below
		}
		for _, dep := range info.depends {
			if !closure[dep.Name] {
				queue = append(queue, dep.Name)
			}
		}
	}

	// 3. Collect closure members that are actually registered.
	active := make([]string, 0, len(closure))
	for name := range closure {
		if infos[name] != nil {
			active = append(active, name)
		}
	}
	sort.Strings(active)

	// 4. Build adjacency list for topological sort (only registered nodes).
	inDegree := make(map[string]int, len(active))
	adj := make(map[string][]string, len(active))
	for _, name := range active {
		inDegree[name] = 0
	}

	for _, name := range active {
		info := infos[name]
		if info == nil {
			continue
		}
		for _, dep := range info.depends {
			depInfo := infos[dep.Name]
			if depInfo == nil {
				continue // dep not registered -> handled in validation
			}
			if !closure[dep.Name] {
				continue // dep not in the closure subset
			}
			adj[name] = append(adj[name], dep.Name)
			inDegree[dep.Name]++
		}
	}

	// 5. Topological sort (Kahn's algorithm) with edges reversed to
	//    dependency->depender, so prerequisites are emitted before the
	//    plugins that require them. "A depends on B" is recorded as B->A:
	//    B has in-degree 0 and A has in-degree 1, so Kahn yields B then A.
	revInDegree := make(map[string]int, len(active))
	revAdj := make(map[string][]string, len(active))
	for _, name := range active {
		revInDegree[name] = 0
	}
	for _, name := range active {
		info := infos[name]
		if info == nil {
			continue
		}
		for _, dep := range info.depends {
			depInfo := infos[dep.Name]
			if depInfo == nil || !closure[dep.Name] {
				continue
			}
			// dep -> depender (prerequisite edge)
			revAdj[dep.Name] = append(revAdj[dep.Name], name)
			revInDegree[name]++
		}
	}

	var sorted []string
	var kahnQueue []string
	for _, name := range active {
		if revInDegree[name] == 0 {
			kahnQueue = append(kahnQueue, name)
		}
	}
	// Sort for deterministic output.
	sort.Strings(kahnQueue)

	for len(kahnQueue) > 0 {
		node := kahnQueue[0]
		kahnQueue = kahnQueue[1:]
		sorted = append(sorted, node)

		for _, next := range revAdj[node] {
			revInDegree[next]--
			if revInDegree[next] == 0 {
				kahnQueue = append(kahnQueue, next)
			}
		}
	}

	// 6. Detect cycles: any active node that didn't make it into sorted.
	sortedSet := make(map[string]bool, len(sorted))
	for _, name := range sorted {
		sortedSet[name] = true
	}
	var cycleNodes []string
	for _, name := range active {
		if !sortedSet[name] {
			cycleNodes = append(cycleNodes, name)
		}
	}
	sort.Strings(cycleNodes)

	res := &DepResolution{}

	if len(cycleNodes) > 0 {
		res.Cycle = cycleNodes
	}

	// 7. Validate deps: missing + version constraints.
	//    Check only for plugins in the closure.
	for _, name := range active {
		info := infos[name]
		if info == nil {
			continue
		}
		for _, dep := range info.depends {
			if res.Requires == nil {
				res.Requires = make(map[string][]string)
			}
			res.Requires[dep.Name] = append(res.Requires[dep.Name], name)

			depInfo := infos[dep.Name]
			if depInfo == nil {
				// Dep not registered: always report as missing.
				res.Missing = append(res.Missing, DepError{
					Plugin:  name,
					Dep:     dep.Name,
					Message: fmt.Sprintf("dependency %q required by %q is not registered", dep.Name, name),
				})
				continue
			}

			// Version constraint check.
			depVer := depInfo.version
			if depVer == "" {
				// No version declared: satisfies any constraint.
				continue
			}

			if dep.MinVersion != "" {
				if cmp := compareVersion(depVer, dep.MinVersion); cmp < 0 {
					res.Version = append(res.Version, DepError{
						Plugin:  name,
						Dep:     dep.Name,
						Message: fmt.Sprintf("%s requires %s >= %s but found %s", name, dep.Name, dep.MinVersion, depVer),
					})
				}
			}
			if dep.MaxVersion != "" {
				if cmp := compareVersion(depVer, dep.MaxVersion); cmp >= 0 {
					res.Version = append(res.Version, DepError{
						Plugin:  name,
						Dep:     dep.Name,
						Message: fmt.Sprintf("%s requires %s < %s but found %s", name, dep.Name, dep.MaxVersion, depVer),
					})
				}
			}
		}
	}

	for _, dependers := range res.Requires {
		sort.Strings(dependers)
	}

	// 8. If no fatal errors, compute the final load order.
	//    Order is the topological sort. AutoAdded are plugins
	//    in sorted that weren't in requested.
	if len(res.Cycle) == 0 {
		reqSet := make(map[string]bool, len(requested))
		for _, name := range requested {
			reqSet[name] = true
		}

		// sorted is in dependency-first order and, absent cycles, already
		// covers every active node: nodes whose deps are unregistered still
		// have in-degree 0 and get emitted. The merge below is defensive:
		// append any active node missing from sorted (shouldn't happen
		// without a cycle).
		for _, name := range active {
			if !sortedSet[name] {
				sorted = append(sorted, name)
			}
		}

		res.Order = sorted

		// 9. Compute BFS levels for parallel startup.
		//    Level 0: plugins with no outgoing deps in the closure (leaf deps).
		//    Level N: plugins whose deps are all at levels < N.
		//    Plugins within the same level can start in parallel.
		level := make(map[string]int, len(sorted))
		depEdges := make(map[string][]string) // name -> its deps in closure

		for _, name := range active {
			info := infos[name]
			if info == nil {
				continue
			}
			for _, dep := range info.depends {
				if closure[dep.Name] && infos[dep.Name] != nil {
					depEdges[name] = append(depEdges[name], dep.Name)
				}
			}
		}

		// Iterate in topological order (deps first), compute level = max(dep level) + 1.
		maxLevel := 0
		for _, name := range sorted {
			lvl := 0
			for _, dep := range depEdges[name] {
				if dl, ok := level[dep]; ok && dl >= lvl {
					lvl = dl + 1
				}
			}
			level[name] = lvl
			if lvl > maxLevel {
				maxLevel = lvl
			}
		}

		// Build level buckets.
		res.Levels = make([][]string, maxLevel+1)
		for _, name := range sorted {
			lvl := level[name]
			res.Levels[lvl] = append(res.Levels[lvl], name)
		}
		// Sort each level deterministically.
		for _, l := range res.Levels {
			sort.Strings(l)
		}

		for _, name := range sorted {
			if !reqSet[name] {
				res.AutoAdded = append(res.AutoAdded, name)
			}
		}
		sort.Strings(res.AutoAdded)
	}

	return res
}

// compareVersion compares two semver-like strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Pre-release suffixes (after the first '-') are stripped before comparison.
func compareVersion(a, b string) int {
	if i := strings.IndexByte(a, '-'); i >= 0 {
		a = a[:i]
	}
	if i := strings.IndexByte(b, '-'); i >= 0 {
		b = b[:i]
	}
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	partsA := splitVersion(a)
	partsB := splitVersion(b)

	for len(partsA) < len(partsB) {
		partsA = append(partsA, 0)
	}
	for len(partsB) < len(partsA) {
		partsB = append(partsB, 0)
	}

	for i := 0; i < len(partsA); i++ {
		if partsA[i] < partsB[i] {
			return -1
		}
		if partsA[i] > partsB[i] {
			return 1
		}
	}
	return 0
}

// splitVersion splits a version string like "1.2.3" into numeric parts.
// Non-numeric segments are treated as 0.
func splitVersion(v string) []int {
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			n = 0
		}
		out = append(out, n)
	}
	return out
}
