package core

import "testing"

// A credential limited to a list of schemas reaches only those, and one
// issued without the list is limited by nothing else here: its roles and
// scopes decide elsewhere.
func TestAuthClaims_AllowsSchema(t *testing.T) {
	cases := []struct {
		name    string
		claims  *AuthClaims
		schema  string
		allowed bool
	}{
		{"nil claims reach nothing", nil, "posts", false},
		{"no list reaches every schema", &AuthClaims{}, "posts", true},
		{"an empty list reaches every schema", &AuthClaims{Schemas: []string{}}, "pages", true},
		{"a listed schema is reached", &AuthClaims{Schemas: []string{"posts", "pages"}}, "pages", true},
		{"an unlisted schema is refused", &AuthClaims{Schemas: []string{"posts"}}, "pages", false},
		{"names compare exactly", &AuthClaims{Schemas: []string{"posts"}}, "Posts", false},
		{"a prefix is not a match", &AuthClaims{Schemas: []string{"post"}}, "posts", false},
		{"an empty schema name is refused by a list", &AuthClaims{Schemas: []string{"posts"}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.claims.AllowsSchema(tc.schema); got != tc.allowed {
				t.Errorf("AllowsSchema(%q) = %v, want %v", tc.schema, got, tc.allowed)
			}
		})
	}
}
