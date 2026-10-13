package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// catalogSource holds three schema definitions in memory. The catalog reads
// no table, so the access decision is the whole subject here.
type catalogSource struct{}

func (catalogSource) List(context.Context) ([]*core.Schema, error) {
	return []*core.Schema{{Name: "blog"}, {Name: "forms"}, {Name: "staff"}}, nil
}

func (catalogSource) GetByName(_ context.Context, name string) (*core.Schema, error) {
	switch name {
	case "blog", "forms", "staff":
		return &core.Schema{Name: name}, nil
	}
	return nil, domain.ErrNotFound
}

// catalogRouter mounts the catalog behind the chain the API router uses for
// a key: API key auth, requireAuth and RequireScoped.
func catalogRouter(perms core.PermissionCheckerProvider, keys map[string]*core.AuthClaims) http.Handler {
	byHash := make(map[string]*core.AuthClaims, len(keys))
	for raw, c := range keys {
		c.UserID, c.APIKeyID, c.TenantID, c.IsAPIKey = raw, raw, "t1", true
		byHash[security.HashKeyPeppered(raw)] = c
	}
	lookup := func(_ context.Context, hash string) (*core.AuthClaims, error) {
		if c, ok := byHash[hash]; ok {
			cp := *c
			return &cp, nil
		}
		return nil, nil
	}
	catalog := &schemaCatalog{source: catalogSource{}, perms: perms}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(tenant.WithID(req.Context(), "t1")))
		})
	})
	r.Use(apimw.APIKeyAuth(lookup))
	r.Use(requireAuth)
	r.Use(apimw.RequireScoped)
	r.Get("/api/v1/schemas", catalog.List)
	r.Get("/api/v1/schemas/{name}", catalog.Get)
	return r
}

func catalogNames(t *testing.T, body []byte) []string {
	t.Helper()
	var got []struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	names := make([]string, 0, len(got))
	for _, s := range got {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names
}

// The catalog describes a schema only to a caller who may read its entries.
// A key limited to blog, by its schema list or by a qualified scope, lists
// blog alone and is refused the others by name, a role no rule grants lists
// nothing, and the rule store failing answers 503 rather than a list that
// might hold what the caller may not see.
func TestSchemaCatalog_HonorsKeySchemasAndRules(t *testing.T) {
	rules := ruleChecker{reads: map[string][]string{
		"viewer": {"blog", "forms"},
	}}
	keys := map[string]*core.AuthClaims{
		"lyv_blog_only":   {Roles: []string{"viewer"}, Scopes: []string{"schemas:read", "content:read"}, Schemas: []string{"blog"}},
		"lyv_every":       {Roles: []string{"viewer"}, Scopes: []string{"schemas:read", "content:read"}},
		"lyv_no_rule":     {Roles: []string{"author"}, Scopes: []string{"schemas:read", "content:read"}},
		"lyv_blog_scope":  {Roles: []string{"viewer"}, Scopes: []string{"schemas:read", "content.blog:read"}},
		"lyv_no_contents": {Roles: []string{"viewer"}, Scopes: []string{"schemas:read"}},
	}
	h := catalogRouter(fakeProvider{rules}, keys)

	t.Run("key limited to blog lists blog alone", func(t *testing.T) {
		rr := call(t, h, http.MethodGet, "lyv_blog_only", "/api/v1/schemas", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, []string{"blog"}, catalogNames(t, rr.Body.Bytes()))
	})

	t.Run("key limited to blog is refused another schema by name", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, call(t, h, http.MethodGet, "lyv_blog_only", "/api/v1/schemas/blog", nil).Code)
		assert.Equal(t, http.StatusForbidden, call(t, h, http.MethodGet, "lyv_blog_only", "/api/v1/schemas/forms", nil).Code)
	})

	t.Run("an empty list reads what the rules allow", func(t *testing.T) {
		rr := call(t, h, http.MethodGet, "lyv_every", "/api/v1/schemas", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, []string{"blog", "forms"}, catalogNames(t, rr.Body.Bytes()))
		assert.Equal(t, http.StatusForbidden, call(t, h, http.MethodGet, "lyv_every", "/api/v1/schemas/staff", nil).Code)
	})

	t.Run("key scoped to content.blog lists blog alone", func(t *testing.T) {
		rr := call(t, h, http.MethodGet, "lyv_blog_scope", "/api/v1/schemas", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, []string{"blog"}, catalogNames(t, rr.Body.Bytes()))
		assert.Equal(t, http.StatusOK, call(t, h, http.MethodGet, "lyv_blog_scope", "/api/v1/schemas/blog", nil).Code)
		rr = call(t, h, http.MethodGet, "lyv_blog_scope", "/api/v1/schemas/forms", nil)
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.Contains(t, rr.Body.String(), "insufficient scope")
	})

	t.Run("key with no content scope lists nothing", func(t *testing.T) {
		rr := call(t, h, http.MethodGet, "lyv_no_contents", "/api/v1/schemas", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Empty(t, catalogNames(t, rr.Body.Bytes()))
		assert.Equal(t, http.StatusForbidden, call(t, h, http.MethodGet, "lyv_no_contents", "/api/v1/schemas/blog", nil).Code)
	})

	t.Run("a refused name answers alike whether or not it exists", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, call(t, h, http.MethodGet, "lyv_blog_only", "/api/v1/schemas/nope", nil).Code)
		assert.Equal(t, http.StatusForbidden, call(t, h, http.MethodGet, "lyv_every", "/api/v1/schemas/nope", nil).Code)
	})

	t.Run("a role no rule grants lists nothing", func(t *testing.T) {
		rr := call(t, h, http.MethodGet, "lyv_no_rule", "/api/v1/schemas", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Empty(t, catalogNames(t, rr.Body.Bytes()))
		assert.Equal(t, http.StatusForbidden, call(t, h, http.MethodGet, "lyv_no_rule", "/api/v1/schemas/blog", nil).Code)
	})

	t.Run("a failing rule store answers 503", func(t *testing.T) {
		failing := catalogRouter(fakeProvider{failingOn{ruleChecker: rules, schema: "forms"}}, map[string]*core.AuthClaims{
			"lyv_every": {Roles: []string{"viewer"}, Scopes: []string{"schemas:read", "content:read"}},
		})
		assert.Equal(t, http.StatusServiceUnavailable, call(t, failing, http.MethodGet, "lyv_every", "/api/v1/schemas", nil).Code)
		assert.Equal(t, http.StatusServiceUnavailable, call(t, failing, http.MethodGet, "lyv_every", "/api/v1/schemas/forms", nil).Code)
	})

	t.Run("no rule engine admits super_admin alone", func(t *testing.T) {
		bare := catalogRouter(nil, map[string]*core.AuthClaims{
			"lyv_every":       {Roles: []string{"viewer"}, Scopes: []string{"schemas:read", "content:read"}},
			"lyv_super_admin": {Roles: []string{core.RoleSuperAdmin}, Scopes: []string{"schemas:read"}},
		})
		rr := call(t, bare, http.MethodGet, "lyv_every", "/api/v1/schemas", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Empty(t, catalogNames(t, rr.Body.Bytes()))
		rr = call(t, bare, http.MethodGet, "lyv_super_admin", "/api/v1/schemas", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, []string{"blog", "forms", "staff"}, catalogNames(t, rr.Body.Bytes()))
		assert.Equal(t, http.StatusNotFound, call(t, bare, http.MethodGet, "lyv_super_admin", "/api/v1/schemas/nope", nil).Code)
	})
}

// The helper every content transport shares refuses the same callers the
// content routes refuse, and tells a refusal from a store failure.
func TestAuthorizeContent_RefusalsAndStoreFailure(t *testing.T) {
	ctx := context.Background()
	rules := ruleChecker{reads: map[string][]string{"viewer": {"blog"}}}

	assert.ErrorIs(t, core.AuthorizeContent(ctx, rules, nil, "blog", "read"), core.ErrNoCaller)
	assert.ErrorIs(t, core.AuthorizeContent(ctx, rules, &core.AuthClaims{Roles: []string{"viewer"}, Schemas: []string{"forms"}}, "blog", "read"), core.ErrSchemaNotAllowed)
	assert.ErrorIs(t, core.AuthorizeContent(ctx, rules, &core.AuthClaims{Roles: []string{"author"}}, "blog", "read"), core.ErrPermissionDenied)
	assert.NoError(t, core.AuthorizeContent(ctx, rules, &core.AuthClaims{Roles: []string{"viewer"}}, "blog", "read"))

	err := core.AuthorizeContent(ctx, failingOn{ruleChecker: rules, schema: "blog"}, &core.AuthClaims{Roles: []string{"viewer"}}, "blog", "read")
	require.Error(t, err)
	for _, refusal := range []error{core.ErrNoCaller, core.ErrSchemaNotAllowed, core.ErrScopeDenied, core.ErrPermissionDenied} {
		assert.False(t, errors.Is(err, refusal))
	}
}
