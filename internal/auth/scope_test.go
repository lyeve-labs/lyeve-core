package auth_test

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestNormalizeScopes(t *testing.T) {
	tests := []struct {
		name   string
		input  []string
		expect []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"single", []string{"content:read"}, []string{"content:read"}},
		{"dedup", []string{"content:read", "content:read"}, []string{"content:read"}},
		{"sort", []string{"media:write", "content:read"}, []string{"content:read", "media:write"}},
		{"whitespace", []string{"  content:read  "}, []string{"content:read"}},
		{"case_insensitive", []string{"Content:Read", "CONTENT:READ"}, []string{"content:read"}},
		{"empty_string_ignored", []string{"", "content:read", ""}, []string{"content:read"}},
		{"wildcards", []string{"*:*", "content:*"}, []string{"*:*", "content:*"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := core.NormalizeScopes(tt.input)
			if len(got) != len(tt.expect) {
				t.Errorf("NormalizeScopes(%v) len=%d, want %d; got=%v", tt.input, len(got), len(tt.expect), got)
				return
			}
			for i := range got {
				if got[i] != tt.expect[i] {
					t.Errorf("NormalizeScopes(%v)[%d] = %q, want %q", tt.input, i, got[i], tt.expect[i])
				}
			}
		})
	}
}

func TestScopeGrantsAll(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		expect bool
	}{
		{"exact_wildcard", []string{"*:*"}, true},
		{"with_other_scopes", []string{"content:read", "*:*"}, true},
		{"no_wildcard", []string{"content:read", "media:write"}, false},
		{"empty", nil, false},
		{"resource_only_wildcard", []string{"content:*"}, false},
		{"action_only_wildcard", []string{"*:read"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := core.ScopeGrantsAll(tt.scopes)
			if got != tt.expect {
				t.Errorf("ScopeGrantsAll(%v) = %v, want %v", tt.scopes, got, tt.expect)
			}
		})
	}
}

func TestScopeGrants(t *testing.T) {
	tests := []struct {
		name     string
		scopes   []string
		resource string
		action   string
		expect   bool
	}{
		{"star_star_matches_anything", []string{"*:*"}, "content", "read", true},
		{"star_star_matches_write", []string{"*:*"}, "media", "delete", true},
		{"resource_wildcard_read", []string{"content:*"}, "content", "read", true},
		{"resource_wildcard_write", []string{"content:*"}, "content", "delete", true},
		{"resource_wildcard_wrong_resource", []string{"content:*"}, "media", "read", false},
		{"action_wildcard_content", []string{"*:read"}, "content", "read", true},
		{"action_wildcard_media", []string{"*:read"}, "media", "read", true},
		{"action_wildcard_wrong_action", []string{"*:read"}, "content", "write", false},
		{"exact_match", []string{"content:read"}, "content", "read", true},
		{"exact_mismatch_resource", []string{"content:read"}, "media", "read", false},
		{"exact_mismatch_action", []string{"content:read"}, "content", "write", false},
		{"multi_finds_match", []string{"content:read", "media:write"}, "media", "write", true},
		{"multi_no_match", []string{"content:read", "media:write"}, "users", "read", false},
		{"case_resource", []string{"Content:read"}, "CONTENT", "read", true},
		{"case_action", []string{"content:Read"}, "content", "READ", true},
		{"empty_scopes", nil, "content", "read", false},
		{"empty_scopes_never_grants", []string{}, "content", "read", false},
		{"malformed_scope_no_colon", []string{"justastring"}, "anything", "read", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := core.ScopeGrants(tt.scopes, tt.resource, tt.action)
			if got != tt.expect {
				t.Errorf("ScopeGrants(%v, %q, %q) = %v, want %v", tt.scopes, tt.resource, tt.action, got, tt.expect)
			}
		})
	}
}

func TestScopeGrantsAny(t *testing.T) {
	scopes := []string{"content:read", "media:write"}
	tests := []struct {
		name     string
		required []core.ScopePair
		expect   bool
	}{
		{"first_matches", []core.ScopePair{{Resource: "content", Action: "read"}, {Resource: "users", Action: "write"}}, true},
		{"second_matches", []core.ScopePair{{Resource: "users", Action: "write"}, {Resource: "media", Action: "write"}}, true},
		{"none_match", []core.ScopePair{{Resource: "users", Action: "read"}, {Resource: "admin", Action: "write"}}, false},
		{"empty_required", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := core.ScopeGrantsAny(scopes, tt.required...)
			if got != tt.expect {
				t.Errorf("ScopeGrantsAny(%v, %v) = %v, want %v", scopes, tt.required, got, tt.expect)
			}
		})
	}
}

func TestBuildScopeRoute(t *testing.T) {
	tests := []struct {
		method   string
		path     string
		resource string
		action   string
	}{
		{"GET", "/api/v1/content/posts", "content.posts", "read"},
		{"POST", "/api/v1/content/posts", "content.posts", "create"},
		{"PUT", "/api/v1/content/posts/123", "content.posts", "update"},
		{"PATCH", "/api/v1/content/posts/123", "content.posts", "update"},
		{"DELETE", "/api/v1/content/posts/123", "content.posts", "delete"},
		{"GET", "/api/v1/media", "media", "read"},
		{"POST", "/api/v1/media/upload", "media.upload", "create"},
		{"GET", "/api/v1/auth/token", "auth.token", "read"},
		{"HEAD", "/api/v1/content/posts", "content.posts", "read"},
		{"OPTIONS", "/api/v1/content/posts", "content.posts", "read"},
		{"GET", "/api/admin/schemas", "schemas", "read"},
		{"POST", "/api/admin/schemas", "schemas", "create"},
		{"GET", "/api/admin/api-keys", "api-keys", "read"},
		{"DELETE", "/api/admin/api-keys/abc", "api-keys.abc", "delete"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			pair := core.BuildScopeRoute(tt.method, tt.path)
			if pair.Resource != tt.resource {
				t.Errorf("resource = %q, want %q", pair.Resource, tt.resource)
			}
			if pair.Action != tt.action {
				t.Errorf("action = %q, want %q", pair.Action, tt.action)
			}
		})
	}
}
