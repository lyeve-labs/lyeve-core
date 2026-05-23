package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// ruleChecker is a rule engine keyed by role and schema, with one field mask
// per schema. It stands in for the plugin that owns the rules, which this
// module cannot import.
type ruleChecker struct {
	reads   map[string][]string // role -> schemas it may read
	updates map[string][]string // role -> schemas it may update
	mask    map[string][]string // schema -> fields hidden from every role
}

func (c ruleChecker) Allowed(_ context.Context, roles []string, schema, action string) (bool, error) {
	var grants map[string][]string
	switch action {
	case "read":
		grants = c.reads
	case "update":
		grants = c.updates
	default:
		return false, nil
	}
	for _, role := range roles {
		for _, s := range grants[role] {
			if s == schema {
				return true, nil
			}
		}
	}
	return false, nil
}

// failingOn is ruleChecker with a rule store that cannot answer for one
// schema.
type failingOn struct {
	ruleChecker
	schema string
}

func (c failingOn) Allowed(ctx context.Context, roles []string, schema, action string) (bool, error) {
	if schema == c.schema {
		return false, errors.New("connection refused")
	}
	return c.ruleChecker.Allowed(ctx, roles, schema, action)
}

func (c ruleChecker) FieldMask(_ context.Context, _ []string, schema string) ([]string, error) {
	return c.mask[schema], nil
}

// keyRouter mounts the content routes behind the middleware chain the API
// router uses for a key: API key auth, requireAuth and RequireScoped. Each
// map key is the raw API key that authenticates as its claims.
func keyRouter(fx schemaContentFixture, keys map[string]*core.AuthClaims) http.Handler {
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

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(tenant.WithID(req.Context(), "t1")))
		})
	})
	r.Use(apimw.APIKeyAuth(lookup))
	r.Use(requireAuth)
	r.Use(apimw.RequireScoped)
	r.Get("/api/v1/content/{schema}", fx.content.List)
	r.Get("/api/v1/content/{schema}/cursor", fx.content.ListCursor)
	r.Get("/api/v1/content/{schema}/{id}", fx.content.Get)
	r.Get("/api/v1/content/{schema}/{id}/revisions", fx.content.ListRevisions)
	r.Get("/api/v1/content/{schema}/{id}/relations/{field}", fx.content.ListRelations)
	r.Put("/api/v1/content/{schema}/{id}/revisions/{rev_id}/restore", fx.content.RestoreRevision)
	return r
}

// call sends one request as the named key, or anonymously when key is empty.
func call(t *testing.T, h http.Handler, method, key, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// An API key reads content through the same middleware chain the API router
// mounts, against real tables on each dialect. Its roles meet the access
// rules and field masks a session's do, and its schema list narrows what
// those rules allow.
func TestContentHandler_APIKey_AccessRulesAndSchemas(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes", "surveys"}, func(t *testing.T, fx schemaContentFixture) {
		noteID := createdID(t, fx.create(t, "notes", map[string]any{
			"title": "First",
			"body":  "first",
		}))
		fx.createSurvey(t)

		// One edit before the rules are installed, so the note has a
		// revision holding the field the mask hides.
		fx.content.revisions = &revisionProvider{s: &memRevisions{}}
		require.Equal(t, http.StatusOK, fx.update(t, "notes", noteID, map[string]any{"body": "first"}).Code)

		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{"reader": {"notes", "surveys"}},
			mask:  map[string][]string{"notes": {"title"}},
		}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"only-notes":   {Roles: []string{"reader"}, Scopes: []string{"content:read"}, Schemas: []string{"notes"}},
			"every-schema": {Roles: []string{"reader"}, Scopes: []string{"content:read"}, Schemas: []string{}},
			"no-rule":      {Roles: []string{"outsider"}, Scopes: []string{"content:read"}},
		})

		cases := []struct {
			name   string
			key    string
			path   string
			status int
			list   bool
		}{
			{"a key reads a schema on its list", "only-notes", "/api/v1/content/notes", http.StatusOK, true},
			{"a key reads one entry on its list", "only-notes", "/api/v1/content/notes/" + noteID, http.StatusOK, false},
			{"a key is refused a schema off its list", "only-notes", "/api/v1/content/surveys", http.StatusForbidden, true},
			{"an empty list reads every schema the rules allow", "every-schema", "/api/v1/content/surveys", http.StatusOK, true},
			{"an empty list still gets the field mask", "every-schema", "/api/v1/content/notes", http.StatusOK, true},
			{"a role no rule grants is refused", "no-rule", "/api/v1/content/notes", http.StatusForbidden, true},
			{"a revision is masked like the entry", "only-notes", "/api/v1/content/notes/" + noteID + "/revisions", http.StatusOK, true},
			{"a key is refused revisions off its list", "only-notes", "/api/v1/content/surveys/" + noteID + "/revisions", http.StatusForbidden, true},
			{"no key is unauthenticated", "", "/api/v1/content/notes", http.StatusUnauthorized, true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rr := call(t, r, http.MethodGet, tc.key, tc.path, nil)
				require.Equal(t, tc.status, rr.Code, rr.Body.String())
				if tc.status != http.StatusOK {
					return
				}

				var entries []map[string]any
				if tc.list {
					require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &entries))
					require.NotEmpty(t, entries)
				} else {
					var one map[string]any
					require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &one))
					entries = []map[string]any{one}
				}
				for _, e := range entries {
					data, ok := e["data"].(map[string]any)
					require.True(t, ok, "entry without data: %v", e)
					assert.NotContains(t, data, "title", "masked field served")
					if e["schema_name"] == "notes" || e["record_id"] != nil {
						assert.Equal(t, "first", data["body"], "unmasked field lost")
					}
				}
			})
		}
	})
}

// ?populate= and ?depth= walk relations into other schemas. Each relation
// they reach is held to the target schema's checks: a key whose list or
// rules do not reach the target gets the entry without the relation filled
// in, and one that does gets the target's mask applied. A key limited to
// responses never reads the surveys they point at through population.
func TestContentHandler_APIKey_PopulateHonorsTheTarget(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"surveys"}, func(t *testing.T, fx schemaContentFixture) {
		surveyID := createdID(t, fx.create(t, "surveys", map[string]any{
			"name":      "Onboarding",
			"slug":      "onboarding-populate",
			"questions": []any{map[string]any{"prompt": "How did you start?", "kind": "text"}},
		}))
		subID := createdID(t, fx.create(t, "survey_responses", map[string]any{
			"survey": surveyID, "data": map[string]any{"answer": "by reading"},
		}))

		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{
				"reader":     {"surveys", "survey_responses"},
				"respondent": {"survey_responses"},
			},
			mask: map[string][]string{"surveys": {"questions"}},
		}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"only-responses":   {Roles: []string{"reader"}, Scopes: []string{"content:read"}, Schemas: []string{"survey_responses"}},
			"every-schema":     {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
			"respondent":       {Roles: []string{"respondent"}, Scopes: []string{"content:read"}},
			"scoped-to-source": {Roles: []string{"reader"}, Scopes: []string{"content.survey_responses:read"}},
		})

		// survey reads the populated relation off a list or a single entry.
		survey := func(t *testing.T, rr *httptest.ResponseRecorder, list bool) any {
			t.Helper()
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var entry map[string]any
			if list {
				var entries []map[string]any
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &entries))
				require.Len(t, entries, 1)
				entry = entries[0]
			} else if strings.Contains(rr.Body.String(), `"next_cursor"`) {
				var page struct {
					Data []map[string]any `json:"data"`
				}
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page))
				require.Len(t, page.Data, 1)
				entry = page.Data[0]
			} else {
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &entry))
			}
			data, ok := entry["data"].(map[string]any)
			require.True(t, ok, "entry without data: %v", entry)
			return data["survey"]
		}

		refused := []struct {
			name, key, path string
			list            bool
		}{
			{"a key off the target's list, wildcard on a list", "only-responses", "/api/v1/content/survey_responses?populate=*", true},
			{"a key off the target's list, named path on one entry", "only-responses", "/api/v1/content/survey_responses/" + subID + "?populate=survey", false},
			{"a key off the target's list, depth on the cursor list", "only-responses", "/api/v1/content/survey_responses/cursor?depth=2", false},
			{"a role the target's rules refuse", "respondent", "/api/v1/content/survey_responses?populate=survey", true},
			{"a key scoped to the source schema alone, wildcard on a list", "scoped-to-source", "/api/v1/content/survey_responses?populate=*", true},
			{"a key scoped to the source schema alone, named path on one entry", "scoped-to-source", "/api/v1/content/survey_responses/" + subID + "?populate=survey", false},
		}
		for _, tc := range refused {
			t.Run(tc.name, func(t *testing.T) {
				got := survey(t, call(t, r, http.MethodGet, tc.key, tc.path, nil), tc.list)
				_, populated := got.(map[string]any)
				assert.False(t, populated, "relation into a schema the key may not read was filled in: %v", got)
			})
		}

		t.Run("a key scoped to both schemas by name reads the relation", func(t *testing.T) {
			both := keyRouter(fx, map[string]*core.AuthClaims{
				"scoped-to-both": {Roles: []string{"reader"}, Scopes: []string{"content.survey_responses:read", "content.surveys:read"}},
			})
			got := survey(t, call(t, both, http.MethodGet, "scoped-to-both", "/api/v1/content/survey_responses?populate=*", nil), true)
			populated, ok := got.(map[string]any)
			require.True(t, ok, "relation the key is scoped to was not filled in: %v", got)
			assert.Equal(t, "Onboarding", populated["name"])
		})

		t.Run("a rule store that fails on the target answers 503", func(t *testing.T) {
			h := *fx.content
			h.perms = fakeProvider{failingOn{ruleChecker: fx.content.perms.(fakeProvider).c.(ruleChecker), schema: "surveys"}}
			broken := fx
			broken.content = &h
			br := keyRouter(broken, map[string]*core.AuthClaims{
				"every-schema": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
			})
			rr := call(t, br, http.MethodGet, "every-schema", "/api/v1/content/survey_responses?populate=survey", nil)
			assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
			assert.NotContains(t, rr.Body.String(), "Onboarding", "the relation was served although its rules could not be read")
		})

		t.Run("a key that reaches the target gets it masked", func(t *testing.T) {
			got := survey(t, call(t, r, http.MethodGet, "every-schema", "/api/v1/content/survey_responses?populate=*", nil), true)
			populated, ok := got.(map[string]any)
			require.True(t, ok, "relation the key may read was not filled in: %v", got)
			assert.Equal(t, "Onboarding", populated["name"])
			assert.NotContains(t, populated, "questions", "the target's masked field was served through populate")
		})
	})
}

// Population that goes two hops holds the second hop to its own target's
// checks. The first target being readable says nothing about the second, and
// ?depth= and a nested wildcard reach it without the request naming it.
func TestContentHandler_APIKey_PopulateSecondHop(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"chain"}, func(t *testing.T, fx schemaContentFixture) {
		regionID := createdID(t, fx.create(t, "regions", map[string]any{"name": "north", "secret": "hq code"}))
		officeID := createdID(t, fx.create(t, "offices", map[string]any{"name": "oslo", "internal": "lease terms", "region": regionID}))
		createdID(t, fx.create(t, "staff", map[string]any{"name": "ada", "office": officeID}))

		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{
				"reader":   {"staff", "offices", "regions"},
				"no-north": {"staff", "offices"},
			},
			mask: map[string][]string{"offices": {"internal"}, "regions": {"secret"}},
		}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"off-the-list": {Roles: []string{"reader"}, Scopes: []string{"content:read"}, Schemas: []string{"staff", "offices"}},
			"rules-refuse": {Roles: []string{"no-north"}, Scopes: []string{"content:read"}},
			"every-schema": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})

		// office reads the first hop off the one staff entry, which every
		// key here may read, and checks its own mask.
		office := func(t *testing.T, key, query string) map[string]any {
			t.Helper()
			rr := call(t, r, http.MethodGet, key, "/api/v1/content/staff?"+query, nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var entries []map[string]any
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &entries))
			require.Len(t, entries, 1)
			data, _ := entries[0]["data"].(map[string]any)
			o, ok := data["office"].(map[string]any)
			require.True(t, ok, "the first hop was not filled in: %v", data)
			assert.Equal(t, "oslo", o["name"])
			assert.NotContains(t, o, "internal", "the first hop's masked field was served")
			return o
		}

		for _, query := range []string{"depth=2", "populate=office.*"} {
			for _, key := range []string{"off-the-list", "rules-refuse"} {
				t.Run(key+" "+query+" leaves the second hop an id", func(t *testing.T) {
					o := office(t, key, query)
					_, populated := o["region"].(map[string]any)
					assert.False(t, populated, "the second hop was filled in: %v", o["region"])
					// SQL Server reads a uniqueidentifier back in upper case,
					// so the id compares without regard to case.
					id, _ := o["region_id"].(string)
					assert.True(t, strings.EqualFold(regionID, id), "the second hop's id was lost: %v", o["region_id"])
				})
			}
			t.Run("every-schema "+query+" gets the second hop masked", func(t *testing.T) {
				o := office(t, "every-schema", query)
				region, ok := o["region"].(map[string]any)
				require.True(t, ok, "the readable second hop was not filled in: %v", o)
				assert.Equal(t, "north", region["name"])
				assert.NotContains(t, region, "secret", "the second hop's masked field was served")
			})
		}
	})
}

// The relation list reads the target schema, so the key needs the target on
// its list and its rules, and the target's mask applies. A restored revision
// is answered with the entry, so the entry's mask applies to it as well.
func TestContentHandler_APIKey_RelationsAndRestore(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"articles"}, func(t *testing.T, fx schemaContentFixture) {
		labelID := createdID(t, fx.create(t, "labels", map[string]any{"name": "news", "secret": "hidden"}))
		articleID := createdID(t, fx.create(t, "articles", map[string]any{"title": "a", "internal": "draft notes"}))

		set := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"ids":["`+labelID+`"]}`))
		set = chiCtx(set.WithContext(fx.ctx), map[string]string{"schema": "articles", "id": articleID, "field": "tags"})
		rr := httptest.NewRecorder()
		fx.content.SetRelations(rr, set)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		revs := &memRevisions{}
		fx.content.revisions = &revisionProvider{s: revs}
		require.Equal(t, http.StatusOK, fx.update(t, "articles", articleID, map[string]any{"title": "b"}).Code)
		require.Len(t, revs.rows, 1)
		revID := revs.rows[0].ID.String()

		fx.content.perms = fakeProvider{ruleChecker{
			reads:   map[string][]string{"reader": {"articles", "labels"}},
			updates: map[string][]string{"reader": {"articles"}},
			mask:    map[string][]string{"labels": {"secret"}, "articles": {"internal"}},
		}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"only-articles": {Roles: []string{"reader"}, Scopes: []string{"content:read", "content:write"}, Schemas: []string{"articles"}},
			"every-schema":  {Roles: []string{"reader"}, Scopes: []string{"content:read", "content:write"}},
			"only-labels":   {Roles: []string{"reader"}, Scopes: []string{"content:read"}, Schemas: []string{"labels"}},
		})
		relations := "/api/v1/content/articles/" + articleID + "/relations/tags"

		t.Run("a key off the target's list is refused the relation list", func(t *testing.T) {
			rr := call(t, r, http.MethodGet, "only-articles", relations, nil)
			assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
		})

		t.Run("a key off the source's list is refused the relation list", func(t *testing.T) {
			rr := call(t, r, http.MethodGet, "only-labels", relations, nil)
			assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
		})

		t.Run("the relation list applies the target's mask", func(t *testing.T) {
			rr := call(t, r, http.MethodGet, "every-schema", relations, nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var out struct {
				Data []struct {
					Data map[string]any `json:"data"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
			require.Len(t, out.Data, 1)
			assert.Equal(t, "news", out.Data[0].Data["name"])
			assert.NotContains(t, out.Data[0].Data, "secret", "the target's masked field was served")
		})

		t.Run("a restore answers with the entry masked", func(t *testing.T) {
			rr := call(t, r, http.MethodPut, "every-schema", "/api/v1/content/articles/"+articleID+"/revisions/"+revID+"/restore", nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			data := dataOf(t, rr)
			assert.Equal(t, "a", data["title"], "the restore did not answer with the restored entry")
			assert.NotContains(t, data, "internal", "the restore served a masked field")
		})
	})
}
