package core

import (
	"fmt"
	"strings"
)

// AdminTokenPrefix opens every admin token. A bearer credential carrying it is
// an admin token and nothing else: the admin router authenticates it, and
// every other router refuses it outright rather than reading the request as
// anonymous.
const AdminTokenPrefix = "lyat_"

// The admin grant catalog. A grant names one thing an admin token may be
// allowed to do on the admin API, and a route is reachable by a token only
// when its declaration names one of these (RouteDecl.AdminGrant). The catalog
// is closed: a declaration or a token naming anything else is refused.
const (
	AdminGrantContentRead   = "content:read"
	AdminGrantContentWrite  = "content:write"
	AdminGrantMediaRead     = "media:read"
	AdminGrantMediaWrite    = "media:write"
	AdminGrantSchemasRead   = "schemas:read"
	AdminGrantFlowsRead     = "flows:read"
	AdminGrantFlowsWrite    = "flows:write"
	AdminGrantWebhooksRead  = "webhooks:read"
	AdminGrantWebhooksWrite = "webhooks:write"
	AdminGrantJobsRead      = "jobs:read"
	AdminGrantJobsWrite     = "jobs:write"
	AdminGrantLogsRead      = "logs:read"
	AdminGrantAuditRead     = "audit:read"
)

// AdminGrantInfo is one catalog entry and the line the admin shows for it.
type AdminGrantInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

var adminGrantCatalog = []AdminGrantInfo{
	{AdminGrantContentRead, "Read content entries and their revisions."},
	{AdminGrantContentWrite, "Create, update, publish and delete content entries."},
	{AdminGrantMediaRead, "List and download media files."},
	{AdminGrantMediaWrite, "Upload, replace and delete media files."},
	{AdminGrantSchemasRead, "Read schema definitions, their row counts and the schema export."},
	{AdminGrantFlowsRead, "Read flows and their runs."},
	{AdminGrantFlowsWrite, "Create, change, activate and delete flows."},
	{AdminGrantWebhooksRead, "Read webhooks and their deliveries."},
	{AdminGrantWebhooksWrite, "Create, change and delete webhooks."},
	{AdminGrantJobsRead, "Read scheduled jobs and their runs."},
	{AdminGrantJobsWrite, "Create, change, trigger and delete scheduled jobs."},
	{AdminGrantLogsRead, "Read and export the instance logs."},
	{AdminGrantAuditRead, "Read and export the audit log."},
}

// AdminGrants returns the catalog in its published order. The slice is a
// copy. Changing it changes nothing.
func AdminGrants() []AdminGrantInfo {
	return append([]AdminGrantInfo(nil), adminGrantCatalog...)
}

// IsAdminGrant reports whether name is in the catalog.
func IsAdminGrant(name string) bool {
	for _, g := range adminGrantCatalog {
		if g.Name == name {
			return true
		}
	}
	return false
}

// SessionOnlyAdminPatterns are the engine's admin routes no admin token may
// ever be granted, whoever owns the token. Each entry is a chi pattern,
// optionally preceded by a method and a space. A {param} segment matches any
// one segment, and a trailing /* matches the path before it and everything
// below it.
//
// They fall in five groups. Who can get in: a token that can mint a
// credential or change an account can make itself permanent. What the
// install is. Destruction across tenants. Personal data in bulk. Process
// internals.
//
// A route the engine does not serve that must stay session only declares
// RouteDecl.SessionOnly, which IsSessionOnlyAdminRoute reads beside them. A
// declaration covers its own route and none below it.
var SessionOnlyAdminPatterns = []string{
	// Who can get in.
	"/api/admin/users/*",
	"/api/admin/roles/*",
	"/api/admin/admin-tokens/*",
	"/api/admin/auth/device/*",
	// What the install is.
	"/api/admin/config/*",
	"/api/admin/plugins/{name}/config/*",
	// Destruction across tenants.
	"/api/admin/schemas/import",
	// Personal data in bulk.
	"/api/admin/gdpr/*",
	// Process internals.
	"/api/admin/debug/pprof/*",
	"POST /api/admin/debug/gc-config",
}

// IsSessionOnlyAdminRoute reports whether the route method and pattern falls
// under SessionOnlyAdminPatterns or is a route a declaration in declared marks
// SessionOnly. A pattern entry covers a shape of routes. A declaration covers
// its own route and no other: the same method and the same pattern, with the
// names of its parameters set aside.
func IsSessionOnlyAdminRoute(method, pattern string, declared ...RouteDecl) bool {
	for _, entry := range SessionOnlyAdminPatterns {
		if sessionOnlyEntryMatches(entry, method, pattern) {
			return true
		}
	}
	for _, rd := range declared {
		if rd.SessionOnly && strings.EqualFold(rd.Method, method) && sameRoutePattern(rd.Pattern, pattern) {
			return true
		}
	}
	return false
}

// sameRoutePattern reports whether two patterns name one route: the same
// segments, where a parameter stands for any other parameter.
func sameRoutePattern(a, b string) bool {
	as, bs := splitPattern(a), splitPattern(b)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if isPatternParam(as[i]) && isPatternParam(bs[i]) {
			continue
		}
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func sessionOnlyEntryMatches(entry, method, pattern string) bool {
	entryMethod := ""
	if m, p, ok := strings.Cut(entry, " "); ok {
		entryMethod, entry = m, p
	}
	if entryMethod != "" && !strings.EqualFold(entryMethod, method) {
		return false
	}
	want := splitPattern(entry)
	got := splitPattern(pattern)
	subtree := len(want) > 0 && want[len(want)-1] == "*"
	if subtree {
		want = want[:len(want)-1]
		if len(got) < len(want) {
			return false
		}
	} else if len(got) != len(want) {
		return false
	}
	for i, seg := range want {
		if isPatternParam(seg) {
			continue
		}
		if seg != got[i] {
			return false
		}
	}
	return true
}

func splitPattern(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func isPatternParam(seg string) bool {
	return len(seg) > 1 && seg[0] == '{' && seg[len(seg)-1] == '}'
}

// ValidateAdminGrants checks the grants a set of route declarations names:
// every one must be in the catalog, and none may sit on a session-only route
// or a GroupSuperAdmin one. A route is session only when a pattern in
// SessionOnlyAdminPatterns covers it, or when a declaration in routes or in
// declared marks it SessionOnly, its own included.
// The admin router refuses to build on the first failure, so a mistake stops
// the boot instead of opening a route or leaving one silently closed.
func ValidateAdminGrants(routes []RouteDecl, declared ...RouteDecl) error {
	all := make([]RouteDecl, 0, len(routes)+len(declared))
	all = append(all, routes...)
	all = append(all, declared...)
	for _, rd := range routes {
		if rd.AdminGrant == "" {
			continue
		}
		if !IsAdminGrant(rd.AdminGrant) {
			return fmt.Errorf("route %s %s declares admin grant %q, which is not in the catalog", rd.Method, rd.Pattern, rd.AdminGrant)
		}
		if IsSessionOnlyAdminRoute(rd.Method, rd.Pattern, all...) {
			return fmt.Errorf("route %s %s is session only and cannot declare admin grant %q", rd.Method, rd.Pattern, rd.AdminGrant)
		}
		// No token carries super_admin, so a grant here could never be used,
		// and a route gated on the role that reaches across tenants is not
		// one to open to a credential bound to one.
		if rd.Group == GroupSuperAdmin {
			return fmt.Errorf("route %s %s is super admin only and cannot declare admin grant %q", rd.Method, rd.Pattern, rd.AdminGrant)
		}
	}
	return nil
}
