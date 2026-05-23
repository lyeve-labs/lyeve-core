package core

import (
	"sort"
	"strings"
)

// API key scope system
//
// Scopes use resource:action format (e.g. "content:read", "media:*", "*:*").
// Wildcards: "*" as resource matches any resource, "*" as action matches any
// action. "*:*" grants access to everything. Scopes are compared
// case-insensitively. Duplicate scopes are deduplicated.
//
// A resource may name one thing under it after a dot: "content.posts:read"
// reaches the posts schema and no other, and "flows.order-sync:create" one
// flow endpoint. A grant of the bare resource covers every name under it, so
// "content:read" reads every schema.
//
// The actions are read, create, update and delete. "write" grants create and
// update together.
//
// An empty scope set means no permissions at all (fail-closed).

// Scope actions a key may be granted. ActionWrite covers ActionCreate and
// ActionUpdate.
const (
	ActionRead   = "read"
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"
	ActionWrite  = "write"
)

// NormalizeScope trims whitespace and lowercases a single scope.
func NormalizeScope(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// NormalizeScopes returns a sorted, deduplicated, normalized list of scopes.
func NormalizeScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(scopes))
	result := make([]string, 0, len(scopes))
	for _, s := range scopes {
		ns := NormalizeScope(s)
		if ns == "" {
			continue
		}
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		result = append(result, ns)
	}
	sort.Strings(result)
	return result
}

// ScopeGrantsAll returns true if the scope set includes the wildcard "*:*"
// which grants access to every resource and action.
func ScopeGrantsAll(scopes []string) bool {
	for _, s := range scopes {
		if NormalizeScope(s) == "*:*" {
			return true
		}
	}
	return false
}

// ScopeGrants returns true if the given scope list grants the requested
// resource:action pair. Wildcards are supported:
//
//	"*:*" - grants everything
//	"content:*" - grants all actions on content and on every schema under it
//	"*:read" - grants read on all resources
//	"content:read" - read on content and on every schema under it
//	"content.posts:read" - read on the posts schema only
//	"content:write" - create and update on content
func ScopeGrants(scopes []string, resource, action string) bool {
	resource = NormalizeScope(resource)
	action = NormalizeScope(action)

	for _, s := range scopes {
		grant, ok := ParseScope(s)
		if !ok {
			continue // malformed scope, skip
		}
		if resourceCovers(grant.Resource, resource) && actionCovers(grant.Action, action) {
			return true
		}
	}
	return false
}

// resourceCovers reports whether a granted resource reaches the requested
// one. A bare resource reaches itself and every name under it. A qualified
// one reaches only itself, so "content.posts" never reaches
// "content.posts.x", a name the path could spell but no schema has.
func resourceCovers(granted, requested string) bool {
	if granted == "*" || granted == requested {
		return true
	}
	if strings.Contains(granted, ".") {
		return false
	}
	return strings.HasPrefix(requested, granted+".")
}

// actionCovers reports whether a granted action reaches the requested one.
func actionCovers(granted, requested string) bool {
	if granted == "*" || granted == requested {
		return true
	}
	return granted == ActionWrite && (requested == ActionCreate || requested == ActionUpdate)
}

// ScopeGrantsAny returns true if the scope list grants at least one of the
// requested resource:action pairs. Useful for endpoints that need at least
// one of several possible scopes.
func ScopeGrantsAny(scopes []string, required ...ScopePair) bool {
	for _, req := range required {
		if ScopeGrants(scopes, req.Resource, req.Action) {
			return true
		}
	}
	return false
}

// ScopePair is a resource:action tuple for use with ScopeGrantsAny.
type ScopePair struct {
	Resource string
	Action   string
}

// ParseScope reads one resource:action scope, normalized. It reports false
// when either half is empty, because a scope with no resource or no action
// names nothing a key could be granted.
func ParseScope(s string) (ScopePair, bool) {
	resource, action, ok := strings.Cut(NormalizeScope(s), ":")
	if !ok || resource == "" || action == "" {
		return ScopePair{}, false
	}
	return ScopePair{Resource: resource, Action: action}, true
}

// BuildScopeRoute maps a request to the scope it needs. The action follows
// the method: GET, HEAD and OPTIONS read, POST creates, PUT and PATCH update,
// DELETE deletes. The resource is the first path segment after the router's
// prefix, qualified by the segment after it when there is one:
// "/api/v1/content/posts/123" needs "content.posts".
func BuildScopeRoute(method, path string) ScopePair {
	resource, name := extractResource(path)
	if name != "" {
		resource += "." + name
	}
	return ScopePair{Resource: resource, Action: methodToAction(method)}
}

func methodToAction(method string) string {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		return ActionRead
	case "POST":
		return ActionCreate
	case "PUT", "PATCH":
		return ActionUpdate
	case "DELETE":
		return ActionDelete
	default:
		return ActionWrite
	}
}

// extractResource extracts the top-level resource from a URL path, and the
// segment that names something under it.
// "/api/v1/content/posts/123" -> "content", "posts"
// "/api/v1/media" -> "media", ""
// "/health" -> "health", ""
func extractResource(path string) (resource, name string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "api" && (parts[1] == "v1" || parts[1] == "admin") && parts[2] != "" {
		if len(parts) >= 4 {
			name = parts[3]
		}
		return parts[2], name
	}
	for _, p := range parts {
		if p != "" {
			return p, ""
		}
	}
	return "unknown", ""
}
