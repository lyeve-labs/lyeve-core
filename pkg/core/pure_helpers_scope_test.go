package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// scope.go

func TestNormalizeScope(t *testing.T) {
	assert.Equal(t, "content:read", NormalizeScope("  Content:Read  "))
	assert.Equal(t, "", NormalizeScope("   "))
	assert.Equal(t, "*:*", NormalizeScope("*:*"))
}

func TestNormalizeScopes(t *testing.T) {
	assert.Nil(t, NormalizeScopes(nil))
	assert.Nil(t, NormalizeScopes([]string{}))

	// Dedup (case-insensitive), drop blanks, and sort.
	got := NormalizeScopes([]string{"B:b", "a:a", "", "A:A", "  ", "b:b"})
	assert.Equal(t, []string{"a:a", "b:b"}, got)
}

func TestScopeGrantsAll(t *testing.T) {
	assert.True(t, ScopeGrantsAll([]string{"content:read", "*:*"}))
	assert.True(t, ScopeGrantsAll([]string{" *:* "})) // normalized
	assert.False(t, ScopeGrantsAll([]string{"content:read", "media:*"}))
	assert.False(t, ScopeGrantsAll(nil))
}

func TestScopeGrants(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		res    string
		action string
		want   bool
	}{
		{"exact match", []string{"content:read"}, "content", "read", true},
		{"exact mismatch action", []string{"content:read"}, "content", "write", false},
		{"resource wildcard", []string{"content:*"}, "content", "delete", true},
		{"action wildcard", []string{"*:read"}, "media", "read", true},
		{"grant all", []string{"*:*"}, "anything", "whatever", true},
		{"case insensitive", []string{"CONTENT:READ"}, "content", "read", true},
		{"malformed skipped", []string{"content", "media:read"}, "media", "read", true},
		{"malformed only", []string{"content"}, "content", "read", false},
		{"no match", []string{"media:read"}, "content", "read", false},
		{"empty set fail-closed", nil, "content", "read", false},
		{"bare resource covers a name under it", []string{"content:read"}, "content.posts", "read", true},
		{"qualified grant reaches its own name", []string{"content.posts:read"}, "content.posts", "read", true},
		{"qualified grant refuses another name", []string{"content.posts:read"}, "content.pages", "read", false},
		{"qualified grant refuses the bare resource", []string{"content.posts:read"}, "content", "read", false},
		{"qualified grant refuses a longer name", []string{"content.posts:read"}, "content.posts.x", "read", false},
		{"bare grant does not cover a prefix that is not a name", []string{"content:read"}, "contents", "read", false},
		{"write covers create", []string{"content:write"}, "content.posts", "create", true},
		{"write covers update", []string{"content:write"}, "content.posts", "update", true},
		{"write does not cover delete", []string{"content:write"}, "content.posts", "delete", false},
		{"create does not cover update", []string{"content.posts:create"}, "content.posts", "update", false},
		{"create does not cover write", []string{"content:create"}, "content", "write", false},
		{"qualified resource wildcard action", []string{"flows.sync:*"}, "flows.sync", "delete", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ScopeGrants(tt.scopes, tt.res, tt.action))
		})
	}
}

func TestScopeGrantsAny(t *testing.T) {
	scopes := []string{"content:read"}
	assert.True(t, ScopeGrantsAny(scopes,
		ScopePair{Resource: "media", Action: "write"},
		ScopePair{Resource: "content", Action: "read"}))
	assert.False(t, ScopeGrantsAny(scopes,
		ScopePair{Resource: "media", Action: "write"}))
	assert.False(t, ScopeGrantsAny(scopes)) // no required pairs
}

func TestBuildScopeRoute(t *testing.T) {
	tests := []struct {
		method, path string
		want         ScopePair
	}{
		{"GET", "/api/v1/content/posts/123", ScopePair{"content.posts", "read"}},
		{"POST", "/api/v1/content/posts", ScopePair{"content.posts", "create"}},
		{"PUT", "/api/v1/flows/order-sync", ScopePair{"flows.order-sync", "update"}},
		{"POST", "/api/v1/media", ScopePair{"media", "create"}},
		{"DELETE", "/api/admin/users/1", ScopePair{"users.1", "delete"}},
		{"PATCH", "/health", ScopePair{"health", "update"}},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, BuildScopeRoute(tt.method, tt.path))
		})
	}
}

func TestMethodToAction(t *testing.T) {
	assert.Equal(t, "read", methodToAction("GET"))
	assert.Equal(t, "read", methodToAction("head"))
	assert.Equal(t, "read", methodToAction("OPTIONS"))
	assert.Equal(t, "create", methodToAction("POST"))
	assert.Equal(t, "update", methodToAction("PUT"))
	assert.Equal(t, "update", methodToAction("PATCH"))
	assert.Equal(t, "delete", methodToAction("DELETE"))
	assert.Equal(t, "write", methodToAction("TRACE")) // default
}

func TestExtractResource(t *testing.T) {
	tests := []struct{ path, resource, name string }{
		{"/api/v1/content/posts", "content", "posts"},
		{"/api/v1/content/posts/1", "content", "posts"},
		{"/api/v1/media", "media", ""},
		{"/api/admin/users/1", "users", "1"},
		{"/api/v1", "api", ""},
		{"/api/v1/", "api", ""},
		{"/health", "health", ""},
		{"", "unknown", ""},
		{"/", "unknown", ""},
	}
	for _, tt := range tests {
		resource, name := extractResource(tt.path)
		assert.Equal(t, tt.resource, resource, tt.path)
		assert.Equal(t, tt.name, name, tt.path)
	}
}

// dialect_helpers.go (thin wrappers over pkg/sqldialect)

func TestDialectHelpers_Upsert(t *testing.T) {
	cfg := sqldialect.UpsertConfig{
		Table:      "t",
		InsertCols: []string{"id", "name"},
		ConflictOn: []string{"id"},
	}
	assert.Equal(t,
		"INSERT INTO t (id, name) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name",
		sqldialect.Upsert("postgres", cfg))
	assert.Equal(t,
		"INSERT INTO t (id, name) VALUES ($1, $2) ON DUPLICATE KEY UPDATE name = $2",
		sqldialect.Upsert("mysql", cfg))
	assert.Equal(t,
		"MERGE t WITH (HOLDLOCK) AS t USING (VALUES ($1, $2)) AS s(id, name) ON s.id = t.id WHEN MATCHED THEN UPDATE SET name = s.name WHEN NOT MATCHED THEN INSERT (id, name) VALUES (s.id, s.name);",
		sqldialect.Upsert("mssql", cfg))
}

func TestDialectHelpers_InsertDoNothing(t *testing.T) {
	assert.Equal(t,
		"INSERT INTO t (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING",
		sqldialect.InsertDoNothing("postgres", "t", []string{"id", "name"}, []string{"id"}))
	assert.Equal(t,
		"INSERT INTO t (id, name) SELECT $1, $2 FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM t AS existing WHERE existing.id = $1)",
		sqldialect.InsertDoNothing("mysql", "t", []string{"id", "name"}, []string{"id"}))
}

func TestDialectHelpers_ILike(t *testing.T) {
	assert.Equal(t, "name ILIKE '%' || $1 || '%'", sqldialect.ILike("postgres", "name", "$1"))
	assert.Equal(t, "name LIKE CONCAT('%', ?, '%')", sqldialect.ILike("mysql", "name", "?"))
	assert.Equal(t, "CHARINDEX(@p1, name) > 0", sqldialect.ILike("mssql", "name", "@p1"))
}

func TestDialectHelpers_Cast(t *testing.T) {
	assert.Equal(t, "x::int", sqldialect.Cast("postgres", "x", "int"))
	assert.Equal(t, "CAST(x AS int)", sqldialect.Cast("postgres", "x", "int", false))
	assert.Equal(t, "CAST(x AS int)", sqldialect.Cast("mysql", "x", "int"))
}

func TestDialectHelpers_LimitOffset(t *testing.T) {
	assert.Equal(t, "LIMIT 10 OFFSET 20", sqldialect.LimitOffset("postgres", 10, 20))
	assert.Equal(t, "LIMIT 10 OFFSET 20", sqldialect.LimitOffset("mysql", 10, 20))
	assert.Equal(t, "OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY", sqldialect.LimitOffset("mssql", 10, 20))
	assert.Equal(t, "OFFSET 0 ROWS FETCH NEXT 1 ROW ONLY", sqldialect.LimitOffset("mssql", 1, 0))
}

func TestDialectHelpers_LimitOffsetPlaceholders(t *testing.T) {
	assert.Equal(t, "LIMIT $3 OFFSET $4", sqldialect.LimitOffsetPlaceholders("postgres", 3, 4))
	assert.Equal(t, "OFFSET CAST(CASE WHEN $3 <= 0 THEN 9223372036854775807 ELSE $4 END AS BIGINT) ROWS FETCH NEXT CAST(CASE WHEN $3 <= 0 THEN 1 ELSE $3 END AS BIGINT) ROWS ONLY", sqldialect.LimitOffsetPlaceholders("mssql", 3, 4))
}

func TestDialectHelpers_QuoteIdentifier(t *testing.T) {
	assert.Equal(t, `"a""b"`, QuoteIdentifier("postgres", `a"b`))
	assert.Equal(t, "`a``b`", QuoteIdentifier("mysql", "a`b"))
	assert.Equal(t, "[a]]b]", QuoteIdentifier("mssql", "a]b"))
}

// auth claims

func TestAuthClaims_HasRole(t *testing.T) {
	var nilClaims *AuthClaims
	assert.False(t, nilClaims.HasRole("admin"), "nil receiver is safe")

	c := &AuthClaims{Roles: []string{"editor", "admin"}}
	assert.True(t, c.HasRole("admin"))
	assert.False(t, c.HasRole("super_admin"))
}

func TestAuthClaims_HasScope(t *testing.T) {
	var nilClaims *AuthClaims
	assert.False(t, nilClaims.HasScope("content", "read"))

	c := &AuthClaims{Scopes: []string{"content:*"}}
	assert.True(t, c.HasScope("content", "write"))
	assert.False(t, c.HasScope("media", "read"))
}

type fakeClaimsProvider struct{ c *AuthClaims }

func (f fakeClaimsProvider) AuthClaims() *AuthClaims { return f.c }

func TestGetClaims(t *testing.T) {
	assert.Nil(t, GetClaims(context.Background()), "no claims in context")

	want := &AuthClaims{UserID: "u1"}
	ctx := context.WithValue(context.Background(), ClaimsKey, want)
	assert.Same(t, want, GetClaims(ctx))

	// A value satisfying the claimsProvider interface is unwrapped.
	prov := fakeClaimsProvider{c: &AuthClaims{UserID: "u2"}}
	ctx = context.WithValue(context.Background(), ClaimsKey, prov)
	got := GetClaims(ctx)
	require.NotNil(t, got)
	assert.Equal(t, "u2", got.UserID)

	// An unrelated type yields nil.
	ctx = context.WithValue(context.Background(), ClaimsKey, "not-claims")
	assert.Nil(t, GetClaims(ctx))
}

func TestRequireSuperAdmin(t *testing.T) {
	newReq := func(claims *AuthClaims) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if claims != nil {
			r = r.WithContext(context.WithValue(r.Context(), ClaimsKey, claims))
		}
		return r
	}

	assert.True(t, RequireSuperAdmin(newReq(&AuthClaims{Roles: []string{"super_admin"}})))
	assert.False(t, RequireSuperAdmin(newReq(&AuthClaims{Roles: []string{"admin"}})))
	assert.False(t, RequireSuperAdmin(newReq(nil)), "no claims -> false")
}
