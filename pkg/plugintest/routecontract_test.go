package plugintest

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// recorder collects what an assertion would have reported, so each rule can be
// shown to fire on a bad declaration and stay quiet on a good one.
type recorder struct{ msgs []string }

func (r *recorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, strings.TrimSpace(fmt.Sprintf(format, args...)))
}
func (r *recorder) Fatalf(format string, args ...any) { r.Errorf(format, args...) }
func (r *recorder) Logf(string, ...any)               {}
func (r *recorder) Helper()                           {}
func (r *recorder) joined() string                    { return strings.Join(r.msgs, "\n") }

var ok = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

func good() []core.RouteDecl {
	return []core.RouteDecl{
		{Method: "GET", Pattern: "/api/admin/probe", Handler: ok, Group: core.GroupAdmin},
		{Method: "POST", Pattern: "/api/admin/probe", Handler: ok, Group: core.GroupAdmin, MaxBodyBytes: 1 << 20},
		{Method: "GET", Pattern: "/api/admin/probe/{id}", Handler: ok, Group: core.GroupAuth, Doc: &core.RouteDoc{
			Summary:   "Read one probe",
			Params:    []core.RouteParam{{Name: "id", In: "path", Format: "uuid"}, {Name: "expand", In: "query", Type: "boolean"}},
			Responses: map[int]string{200: "OK", 404: "Not found"},
		}},
		{Method: "POST", Pattern: "/api/v1/probe/hooks/{id}", Handler: ok, Group: core.GroupPublic, SelfScoping: true, RateLimit: &core.RouteRateLimit{Rate: 10, Burst: 20}},
		{Method: "POST", Pattern: "/api/v1/probe/query", Handler: ok, Group: core.GroupAuth, Scope: "probe:read"},
	}
}

func TestAssertRouteContract_AcceptsAWellFormedSurface(t *testing.T) {
	r := &recorder{}
	AssertRouteContract(r, "probe", good())
	if r.joined() != "" {
		t.Fatalf("a correct declaration was rejected:\n%s", r.joined())
	}
}

func TestAssertRouteContract_EachRuleFires(t *testing.T) {
	cases := []struct {
		name  string
		decl  core.RouteDecl
		wants string
	}{
		{"nil handler", core.RouteDecl{Method: "GET", Pattern: "/api/x", Group: core.GroupAdmin}, "nil handler"},
		{"unmounted method", core.RouteDecl{Method: "TRACE", Pattern: "/api/x", Handler: ok, Group: core.GroupAdmin}, "does not mount"},
		{"unknown group", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: "editor"}, "not one the engine gates on"},
		{"relative pattern", core.RouteDecl{Method: "GET", Pattern: "api/x", Handler: ok, Group: core.GroupAdmin}, "does not start with /"},
		{"empty segment", core.RouteDecl{Method: "GET", Pattern: "/api//x", Handler: ok, Group: core.GroupAdmin}, "empty segment"},
		{"unbalanced braces", core.RouteDecl{Method: "GET", Pattern: "/api/{id", Handler: ok, Group: core.GroupAdmin}, "braces are unbalanced"},
		{"limit on a parameter", core.RouteDecl{Method: "PUT", Pattern: "/api/x/{id}", Handler: ok, Group: core.GroupAdmin, MaxBodyBytes: 1 << 20}, "the limit is ignored"},
		{"self-scoping off public", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupAuth, SelfScoping: true}, "only relaxes the public-route Host rule"},
		{"scope off auth", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupPublic, Scope: "x:read"}, "only authenticated routes read a declared scope"},
		{"scope not resource:action", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupAuth, Scope: "x"}, "not resource:action"},
		{"rate limit off public", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupAdmin, RateLimit: &core.RouteRateLimit{Rate: 1, Burst: 2}}, "wraps only public routes"},
		{"rate limit with no rate", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupPublic, RateLimit: &core.RouteRateLimit{Rate: 0, Burst: 2}}, "both must be positive"},
		{"rate limit with no burst", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupPublic, RateLimit: &core.RouteRateLimit{Rate: 1, Burst: 0}}, "both must be positive"},
		{"doc parameter nowhere", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupAdmin, Doc: &core.RouteDoc{Params: []core.RouteParam{{Name: "q", In: "body"}}}}, "not path, query, header or cookie"},
		{"doc parameter unnamed", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupAdmin, Doc: &core.RouteDoc{Params: []core.RouteParam{{In: "query"}}}}, "with no name"},
		{"doc path parameter missing", core.RouteDecl{Method: "GET", Pattern: "/api/x/{id}", Handler: ok, Group: core.GroupAdmin, Doc: &core.RouteDoc{Params: []core.RouteParam{{Name: "slug", In: "path"}}}}, "the pattern does not hold"},
		{"doc response not a status", core.RouteDecl{Method: "GET", Pattern: "/api/x", Handler: ok, Group: core.GroupAdmin, Doc: &core.RouteDoc{Responses: map[int]string{42: "odd"}}}, "not an HTTP status"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{}
			AssertRouteContract(r, "probe", []core.RouteDecl{c.decl})
			if !strings.Contains(r.joined(), c.wants) {
				t.Errorf("expected a complaint containing %q, got:\n%s", c.wants, r.joined())
			}
		})
	}
}

func TestAssertRouteContract_CatchesADuplicateAndAnEmptySurface(t *testing.T) {
	dup := append(good(), core.RouteDecl{Method: "GET", Pattern: "/api/admin/probe", Handler: ok, Group: core.GroupAdmin})
	r := &recorder{}
	AssertRouteContract(r, "probe", dup)
	if !strings.Contains(r.joined(), "declared twice") {
		t.Errorf("a second registration of one method and pattern is unreachable and must be reported, got:\n%s", r.joined())
	}

	empty := &recorder{}
	AssertRouteContract(empty, "probe", nil)
	if !strings.Contains(empty.joined(), "declares no routes") {
		t.Errorf("an empty surface must be reported rather than passing silently, got:\n%s", empty.joined())
	}
}

func TestRoutePatterns_IsSortedAndDeduplicated(t *testing.T) {
	got := RoutePatterns(good())
	want := []string{"/api/admin/probe", "/api/admin/probe/{id}", "/api/v1/probe/hooks/{id}", "/api/v1/probe/query"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A grant on a route the plugin marks session only would stop the admin
// router from building, so the plugin's own suite reports it first, whether
// the mark is on the granted declaration or on another one in the set.
func TestAssertRouteContract_RefusesAGrantOnASessionOnlyRoute(t *testing.T) {
	own := &recorder{}
	AssertRouteContract(own, "probe", []core.RouteDecl{
		{Method: "POST", Pattern: "/api/admin/probe/{id}/rotate", Handler: ok, Group: core.GroupAdmin, SessionOnly: true, AdminGrant: core.AdminGrantContentWrite},
	})
	if !strings.Contains(own.joined(), "session only") {
		t.Errorf("expected a session-only complaint, got:\n%s", own.joined())
	}

	other := &recorder{}
	RequireNoSessionOnlyGrants(other, []core.RouteDecl{
		{Method: "POST", Pattern: "/api/admin/probe/{id}/rotate", Handler: ok, Group: core.GroupAdmin, SessionOnly: true},
		{Method: "POST", Pattern: "/api/admin/probe/{probe}/rotate", Handler: ok, Group: core.GroupAdmin, AdminGrant: core.AdminGrantContentWrite},
	})
	if !strings.Contains(other.joined(), "session only") {
		t.Errorf("expected a session-only complaint, got:\n%s", other.joined())
	}
}
