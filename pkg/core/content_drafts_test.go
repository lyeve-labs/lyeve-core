package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// actionChecker grants each action to the roles listed for it.
type actionChecker map[string]map[string]bool

func (c actionChecker) Allowed(_ context.Context, roles []string, _, action string) (bool, error) {
	for _, r := range roles {
		if c[action][r] {
			return true, nil
		}
	}
	return false, nil
}

func TestCanReadDrafts_SessionsAndKeys(t *testing.T) {
	checker := actionChecker{
		"read":   {"viewer": true, "editor": true, "admin": true},
		"update": {"editor": true, "admin": true, "writer": true},
	}
	cases := []struct {
		name   string
		claims *AuthClaims
		want   bool
	}{
		{"no caller", nil, false},
		{"an editor session", &AuthClaims{Roles: []string{"editor"}}, true},
		{"a viewer session", &AuthClaims{Roles: []string{"viewer"}}, false},
		{"a session that may update and not read", &AuthClaims{Roles: []string{"writer"}}, false},
		{"an editor key scoped to read", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content:read"}, IsAPIKey: true}, false},
		{"an editor key scoped to read everything", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"*:read"}, IsAPIKey: true}, false},
		{"an editor key scoped to write content", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content:read", "content:write"}, IsAPIKey: true}, true},
		{"an editor key scoped to read and update this schema", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content.posts:read", "content.posts:update"}, IsAPIKey: true}, true},
		{"an editor key scoped to update this schema only", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content.posts:update"}, IsAPIKey: true}, false},
		{"an editor key scoped to write content without read", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content:write"}, IsAPIKey: true}, false},
		{"an editor key scoped to write another schema", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content.other:write"}, IsAPIKey: true}, false},
		{"an editor key scoped to create only", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content:create"}, IsAPIKey: true}, false},
		{"an editor key with every scope", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"*:*"}, IsAPIKey: true}, true},
		{"an admin key without scopes", &AuthClaims{Roles: []string{"admin"}, IsAPIKey: true}, true},
		{"a viewer key scoped to write", &AuthClaims{Roles: []string{"viewer"}, Scopes: []string{"content:write"}, IsAPIKey: true}, false},
		{"an editor limited to another schema", &AuthClaims{Roles: []string{"editor"}, Schemas: []string{"other"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CanReadDrafts(context.Background(), checker, tc.claims, "posts"))
		})
	}
}

// An event that carries no status fails closed on a schema with draft and
// publish, where it may be a writer's partial payload about a draft, and is
// public on any other schema.
func TestContentEventPublished_FailsClosedOnDraftPublishSchemas(t *testing.T) {
	for _, data := range []map[string]any{{"title": "partial"}, {"_status": nil}, {"_status": "draft"}, {"_status": "archived"}, {"_status": 1}} {
		assert.False(t, ContentEventPublished(true, data), "%v", data)
		assert.True(t, ContentEventPublished(false, data), "%v", data)
	}
	assert.True(t, ContentEventPublished(true, map[string]any{"_status": "published"}))
}

func TestContentPublished_ReadsTheStatusKey(t *testing.T) {
	assert.True(t, ContentPublished(map[string]any{"title": "no status column"}))
	assert.True(t, ContentPublished(map[string]any{"_status": nil}))
	assert.True(t, ContentPublished(map[string]any{"_status": "published"}))
	assert.False(t, ContentPublished(map[string]any{"_status": "draft"}))
	assert.False(t, ContentPublished(map[string]any{"_status": "archived"}))
}
