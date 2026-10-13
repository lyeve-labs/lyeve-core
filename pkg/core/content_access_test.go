package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// allowAll is a rule engine that grants every role every action, so a
// refusal from AuthorizeContent over it can only come from the credential.
type allowAll struct{ asked *int }

func (a allowAll) Allowed(context.Context, []string, string, string) (bool, error) {
	if a.asked != nil {
		*a.asked++
	}
	return true, nil
}

// failingRules is a rule store that cannot answer.
type failingRules struct{}

func (failingRules) Allowed(context.Context, []string, string, string) (bool, error) {
	return false, errors.New("rule store unavailable")
}

// A key is held to its content scopes on every schema a transport reaches,
// not only on the one a route's path names. A key scoped to one schema is
// refused another even when the rules would let its roles read it.
func TestAuthorizeContent_HoldsAKeyToItsScopes(t *testing.T) {
	key := func(scopes ...string) *AuthClaims {
		return &AuthClaims{UserID: "key-1", APIKeyID: "key-1", Roles: []string{"editor"}, Scopes: scopes, IsAPIKey: true}
	}
	for _, tc := range []struct {
		name   string
		claims *AuthClaims
		schema string
		action string
		want   error
	}{
		{"a key scoped to posts is refused authors", key("content.posts:read"), "authors", ActionRead, ErrScopeDenied},
		{"a key scoped to posts reads posts", key("content.posts:read"), "posts", ActionRead, nil},
		{"a qualified read grants no update", key("content.posts:read"), "posts", ActionUpdate, ErrScopeDenied},
		{"a qualified write grants create", key("content.posts:write"), "posts", ActionCreate, nil},
		{"a qualified write reaches no other schema", key("content.posts:write"), "authors", ActionCreate, ErrScopeDenied},
		{"the bare content scope covers every schema", key("content:read"), "authors", ActionRead, nil},
		{"a schemas scope reads no content", key("schemas:read"), "posts", ActionRead, ErrScopeDenied},
		{"a key with no scopes is refused", key(), "posts", ActionRead, ErrScopeDenied},
		{"the wildcard covers everything", key("*:*"), "authors", ActionDelete, nil},
		{"scopes compare without case", key("Content.Posts:Read"), "posts", ActionRead, nil},
		{"an admin key answers to its role", &AuthClaims{Roles: []string{RoleAdmin}, IsAPIKey: true}, "authors", ActionRead, nil},
		{"a super_admin key answers to its role", &AuthClaims{Roles: []string{RoleSuperAdmin}, IsAPIKey: true}, "authors", ActionDelete, nil},
		{"a session carries no scopes and is judged by its roles", &AuthClaims{UserID: "u1", Roles: []string{"editor"}}, "authors", ActionRead, nil},
		{"the schema list refuses before the scopes are read", &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content:read"}, Schemas: []string{"posts"}, IsAPIKey: true}, "authors", ActionRead, ErrSchemaNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := AuthorizeContent(context.Background(), allowAll{}, tc.claims, tc.schema, tc.action)
			if tc.want == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

// A key its scopes refuse is refused before the rule store is asked, so a
// store that cannot answer does not turn the refusal into an outage, and one
// that answers yes does not turn it into a grant.
func TestAuthorizeContent_ScopeRefusalPrecedesTheRules(t *testing.T) {
	claims := &AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content.posts:read"}, IsAPIKey: true}
	asked := 0
	assert.ErrorIs(t, AuthorizeContent(context.Background(), allowAll{asked: &asked}, claims, "authors", ActionRead), ErrScopeDenied)
	assert.Zero(t, asked, "the rules were asked about a schema the key's scopes refuse")
	assert.ErrorIs(t, AuthorizeContent(context.Background(), failingRules{}, claims, "authors", ActionRead), ErrScopeDenied)
}
