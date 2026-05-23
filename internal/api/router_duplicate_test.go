package api

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// routeReg matches chi route registration calls within an r.With(...).
// Captures method and pattern from lines like:
//
//	r.With(requireRole("admin")).Get("/openapi.json", openAPIHandler(...))
var routeReg = regexp.MustCompile(`\.(Get|Post|Put|Patch|Delete|Head|Options)\(\s*"([^"]+)"\s*,`)

func readSource(t *testing.T, relPath string) string {
	t.Helper()

	_, thisFile, _, _ := runtime.Caller(0)
	srcDir := filepath.Dir(thisFile)
	path := filepath.Join(srcDir, relPath)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", relPath, err)
	}
	return string(data)
}

// Scans router.go for duplicate method+pattern registrations inside the
// same route Group. A duplicate registration is silently overwritten by
// chi (setEndpoint) and invisible to chi.Walk,
// so a source-level check is needed.
func TestNoDuplicateRouteRegistrations(t *testing.T) {
	src := readSource(t, "router.go")
	sc := bufio.NewScanner(strings.NewReader(src))

	type group struct {
		depth  int
		routes map[string]int // "METHOD /path" -> count
	}

	var (
		groups []*group
		cur    *group
	)

	groupOpen := regexp.MustCompile(`\.Group\(\s*func\s*\(`)

	depth := 0
	for sc.Scan() {
		line := sc.Text()

		openBraces := strings.Count(line, "{")
		closeBraces := strings.Count(line, "}")
		depth += openBraces - closeBraces

		if groupOpen.MatchString(line) {
			cur = &group{depth: depth, routes: make(map[string]int)}
			groups = append(groups, cur)
			continue
		}

		if cur != nil && depth < cur.depth {
			cur = nil
		}

		if cur == nil {
			continue
		}

		matches := routeReg.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		method := strings.ToUpper(matches[1])
		pattern := matches[2]
		key := method + " " + pattern
		cur.routes[key]++
	}

	var duplicates []string
	for i, g := range groups {
		for route, count := range g.routes {
			if count > 1 {
				duplicates = append(duplicates, fmt.Sprintf("Group %d: %s registered %d times", i+1, route, count))
			}
		}
	}

	if len(duplicates) > 0 {
		for _, d := range duplicates {
			t.Errorf("duplicate route registration: %s", d)
		}
	}
}

// engineRouteConflicts flags plugin routes that collide with engine-owned
// routes (dead-route shadowing) and leaves genuinely additive routes
// alone. Guards against a plugin re-declaring a route the engine already
// serves, which chi silently overwrites.
func TestEngineRouteConflictGuard(t *testing.T) {
	t.Run("admin", func(t *testing.T) {
		routes := []plugin.PluginRoutes{
			{
				Name: "impostor",
				Routes: []plugin.RouteDecl{
					{Method: "GET", Pattern: "/api/admin/users", Group: plugin.GroupAdmin},
					{Method: "POST", Pattern: "/api/admin/users", Group: plugin.GroupSuperAdmin},
					{Method: "POST", Pattern: "/api/admin/users/{id}/preview", Group: plugin.GroupAdmin},
					{Method: "POST", Pattern: "/api/admin/users/migrate", Group: plugin.GroupSuperAdmin},
				},
			},
		}

		conflicts := engineRouteConflicts(routes, "/api/admin")

		got := make(map[string]bool, len(conflicts))
		for _, c := range conflicts {
			got[c.method+" "+c.pattern] = true
		}
		want := []string{
			"GET /api/admin/users",
			"POST /api/admin/users",
		}
		if len(conflicts) != len(want) {
			t.Fatalf("expected %d conflicts, got %d: %+v", len(want), len(conflicts), conflicts)
		}
		for _, w := range want {
			if !got[w] {
				t.Errorf("expected conflict %q not detected", w)
			}
		}
	})

	t.Run("api_v1_content", func(t *testing.T) {
		routes := []plugin.PluginRoutes{
			{
				Name: "rogue",
				Routes: []plugin.RouteDecl{
					{Method: "POST", Pattern: "/api/v1/content/{schema}", Group: plugin.GroupPublic},
					{Method: "POST", Pattern: "/api/v1/widgets", Group: plugin.GroupPublic},
				},
			},
		}
		conflicts := engineRouteConflicts(routes, "/api/v1")
		if len(conflicts) != 1 || conflicts[0].pattern != "/api/v1/content/{schema}" {
			t.Fatalf("expected exactly the content collision, got %+v", conflicts)
		}
	})
}

// AllowedHosts middleware must be wired before HTTPSRedirect in router.go.
// Without this ordering, an attacker can poison the redirect target by
// setting a malicious Host header.
func TestAllowedHostsBeforeHTTPSRedirect(t *testing.T) {
	src := readSource(t, "router.go")

	ahRe := regexp.MustCompile(`apimw\.AllowedHosts\(`)
	hrRe := regexp.MustCompile(`apimw\.HTTPSRedirect\(`)

	type site struct {
		line int
		name string
	}
	var sites []site
	for i, line := range strings.Split(src, "\n") {
		if ahRe.MatchString(line) {
			sites = append(sites, site{line: i + 1, name: "AllowedHosts"})
		}
		if hrRe.MatchString(line) {
			sites = append(sites, site{line: i + 1, name: "HTTPSRedirect"})
		}
	}

	for i, s := range sites {
		if s.name == "HTTPSRedirect" {
			if i == 0 || sites[i-1].name != "AllowedHosts" {
				t.Errorf("line %d: HTTPSRedirect has no preceding AllowedHosts", s.line)
			} else {
				t.Logf("line %d: AllowedHosts (line %d) precedes HTTPSRedirect", s.line, sites[i-1].line)
			}
		}
	}
}
