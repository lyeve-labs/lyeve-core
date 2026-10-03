package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

type updateChecker struct{ roles map[string]bool }

func (c updateChecker) Allowed(_ context.Context, roles []string, _, action string) (bool, error) {
	if action != "update" {
		return true, nil
	}
	for _, r := range roles {
		if c.roles[r] {
			return true, nil
		}
	}
	return false, nil
}

func TestCanReadDrafts_SessionsAndKeys(t *testing.T) {
	checker := updateChecker{roles: map[string]bool{"editor": true, "admin": true}}
	cases := []struct {
		name   string
		claims *AuthClaims
		want   bool
	}{
		{"no caller", nil, false},
		{"an editor session", &AuthClaims{Roles: []string{"editor"}}, true},
		{"a viewer session", &AuthClaims{Roles: []string{"viewer"}}, false},
		{"an editor key scoped to read", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content:read"}, IsAPIKey: true}, false},
		{"an editor key scoped to read everything", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"*:read"}, IsAPIKey: true}, false},
		{"an editor key scoped to write content", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content:read", "content:write"}, IsAPIKey: true}, true},
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

func TestContentPublished_ReadsTheStatusKey(t *testing.T) {
	assert.True(t, ContentPublished(map[string]any{"title": "no status column"}))
	assert.True(t, ContentPublished(map[string]any{"_status": nil}))
	assert.True(t, ContentPublished(map[string]any{"_status": "published"}))
	assert.False(t, ContentPublished(map[string]any{"_status": "draft"}))
	assert.False(t, ContentPublished(map[string]any{"_status": "archived"}))
}
