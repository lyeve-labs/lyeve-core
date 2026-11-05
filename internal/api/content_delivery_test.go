package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

const (
	deliveryHostA      = "a.example.test"
	deliveryHostB      = "b.example.test"
	deliveryPublicCC   = "public, max-age=60, s-maxage=300, stale-while-revalidate=60, stale-if-error=600"
	deliveryPostsPath  = "/api/v1/delivery/dl_posts"
	deliveryVaultsPath = "/api/v1/delivery/dl_vaults"
)

// deliverySource is the schema engine's source read the way a tenant's own
// definitions are: unmarked names, per tenant, the schemas whose delivery
// mark that tenant has not set, failing names the ones whose definition
// cannot be read, and fold matches a name the way a case-insensitive
// collation does.
type deliverySource struct {
	inner    core.SchemaSource
	unmarked map[string]bool
	failing  map[string]bool
	fold     bool
}

func (s *deliverySource) GetByName(ctx context.Context, name string) (*core.Schema, error) {
	if s.failing[name] {
		return nil, errors.New("definition store unreachable")
	}
	sc, err := s.inner.GetByName(ctx, name)
	if err != nil && s.fold {
		all, lerr := s.inner.List(ctx)
		if lerr != nil {
			return nil, lerr
		}
		for _, cand := range all {
			if strings.EqualFold(cand.Name, name) {
				sc, err = cand, nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if s.unmarked[core.TenantIDFromCtx(ctx)+"/"+sc.Name] {
		cp := *sc
		cp.PublicDelivery = false
		sc = &cp
	}
	return sc, nil
}

func (s *deliverySource) List(ctx context.Context) ([]*core.Schema, error) {
	return s.inner.List(ctx)
}

// hostTenants stands in for the domain routing a plugin contributes: it
// names the tenant a registered host belongs to and nothing else.
func hostTenants(hosts map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slug, ok := hosts[r.Host]; ok {
				r = r.WithContext(core.WithTenantID(r.Context(), slug))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// deliveryRig is the content API router of a multi-tenant install with two
// registered hosts, over the fixture's database. Writes go through the
// fixture's handler, which shares the router's content store and its caches.
type deliveryRig struct {
	fx     schemaContentFixture
	cfg    *config.Config
	src    *deliverySource
	hooks  *hooks.Registry
	pool   *failingReads
	router http.Handler
}

// failingReads is the fixture's database with reads of one table made to
// fail, the way a lost connection or a dropped table fails them.
type failingReads struct {
	db.DB
	mu    sync.Mutex
	table string
}

func (p *failingReads) fail(table string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.table = table
}

func (p *failingReads) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	p.mu.Lock()
	table := p.table
	p.mu.Unlock()
	if table != "" && strings.Contains(query, table) {
		return nil, errors.New("read of " + table + " failed")
	}
	return p.DB.Query(ctx, query, args...)
}

func newDeliveryRig(t *testing.T, fx schemaContentFixture, setup func(*config.Config, *deliverySource), opts ...RouterOption) *deliveryRig {
	t.Helper()
	cfg := testConfig()
	cfg.MultiTenant = true
	src := &deliverySource{inner: fx.content.schemas, unmarked: map[string]bool{}, failing: map[string]bool{}}
	if setup != nil {
		setup(cfg, src)
	}
	reg := hooks.NewRegistry()
	pool := &failingReads{DB: fx.pool}
	var store *db.ContentStore
	opts = append([]RouterOption{
		WithLifetime(testLifetime(t)),
		WithSchemaSource(src),
		WithContentRules(fx.rules),
		WithPreAuthMiddleware(hostTenants(map[string]string{deliveryHostA: "t1", deliveryHostB: "t2"})),
		WithTenantRoster(func(context.Context) (int, error) { return 2, nil }),
		WithContentStoreSink(func(cs *db.ContentStore) { store = cs }),
	}, opts...)
	router, err := NewAPIRouter(pool, cfg, reg, opts...)
	require.NoError(t, err)
	require.NotNil(t, store)
	fx.content.store = store
	fx.content.schemas = src
	return &deliveryRig{fx: fx, cfg: cfg, src: src, hooks: reg, pool: pool, router: router}
}

// get sends an anonymous GET to host, after set has added what the case needs.
func (rg *deliveryRig) get(t *testing.T, host, target string, set func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	req.RemoteAddr = "198.51.100.7:4000"
	if set != nil {
		set(req)
	}
	rec := httptest.NewRecorder()
	rg.router.ServeHTTP(rec, req)
	return rec
}

// post writes one entry of schema in tenant, with its status when the
// schema has draft and publish, and returns its id.
func (rg *deliveryRig) post(t *testing.T, tenantID, schema, status string, data map[string]any) string {
	t.Helper()
	if status != "" {
		data["_status"] = status
	}
	ctx := asSuperAdmin(tenant.WithID(context.Background(), tenantID))
	rr := rg.fx.send(t, ctx, http.MethodPost, "/api/v1/content/"+schema, map[string]string{"schema": schema},
		map[string]any{"data": data}, rg.fx.content.Create)
	return createdID(t, rr)
}

// deliveredIDs decodes a 200 list and returns its ids.
func deliveredIDs(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	return listedIDs(t, rr)
}

// deliveredRows decodes a 200 list into each entry's data.
func deliveredRows(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var items []struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &items), rr.Body.String())
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.Data)
	}
	return out
}

// A type its tenant has not marked is absent from delivery, by list and by
// id, as a type that does not exist is.
func TestDelivery_UnmarkedSchemaAnswersNotFound(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		vault := rg.post(t, "t1", "dl_vaults", "", map[string]any{"code": "v-code"})
		rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "open"})

		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath, nil).Code)
		for _, path := range []string{deliveryVaultsPath, deliveryVaultsPath + "/" + vault, "/api/v1/delivery/nosuch"} {
			rr := rg.get(t, deliveryHostA, path, nil)
			assert.Equal(t, http.StatusNotFound, rr.Code, path)
			assert.NotContains(t, rr.Body.String(), "v-code", path)
			assert.Contains(t, rr.Header().Get("Cache-Control"), "no-store", path)
		}
	})
}

// A list serves published entries and nothing else: no draft, no archived
// entry, no entry in the trash.
func TestDelivery_ListServesPublishedEntriesOnly(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		pub := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})
		rg.post(t, "t1", "dl_posts", "draft", map[string]any{"title": "draft"})
		rg.post(t, "t1", "dl_posts", "archived", map[string]any{"title": "archived"})
		trashed := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "trashed"})
		rr := fx.send(t, fx.ctx, http.MethodDelete, "/api/v1/content/dl_posts/"+trashed,
			map[string]string{"schema": "dl_posts", "id": trashed}, nil, fx.content.Delete)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		assert.ElementsMatch(t, []string{pub}, deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath+"?limit=50", nil)))
	})
}

// A draft read by id answers what a missing id does, so the read does not
// confirm that the draft exists.
func TestDelivery_GetOfDraftAnswersNotFound(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		pub := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})
		draft := rg.post(t, "t1", "dl_posts", "draft", map[string]any{"title": "draft"})

		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+pub, nil).Code)
		missing := rg.get(t, deliveryHostA, deliveryPostsPath+"/"+uuid.NewString(), nil)
		drafted := rg.get(t, deliveryHostA, deliveryPostsPath+"/"+draft, nil)
		assert.Equal(t, http.StatusNotFound, drafted.Code, drafted.Body.String())
		assert.Equal(t, missing.Code, drafted.Code)
		assert.JSONEq(t, stripRequestID(t, missing.Body.String()), stripRequestID(t, drafted.Body.String()))
	})
}

// A field marked private never leaves the delivery routes, on a list, on a
// read by id, or inside a populated record of a marked type.
func TestDelivery_PrivateFieldIsLeftOut(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		author := rg.post(t, "t1", "dl_people", "published", map[string]any{"name": "Ada", "email": "ada@private.test"})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "secret": "hush", "author": author})

		list := rg.get(t, deliveryHostA, deliveryPostsPath+"?populate=author", nil)
		rows := deliveredRows(t, list)
		require.Len(t, rows, 1)
		assert.NotContains(t, rows[0], "secret")
		populated, _ := rows[0]["author"].(map[string]any)
		require.NotNil(t, populated, list.Body.String())
		assert.Equal(t, "Ada", populated["name"])
		assert.NotContains(t, populated, "email")

		one := rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id, nil)
		require.Equal(t, http.StatusOK, one.Code, one.Body.String())
		assert.NotContains(t, dataOf(t, one), "secret")
		for _, body := range []string{list.Body.String(), one.Body.String()} {
			assert.NotContains(t, body, "hush")
			assert.NotContains(t, body, "ada@private.test")
		}
	})
}

// A filter on a private field, or on a relation into a type that is not
// marked, answers exactly what a filter on a field the type does not have
// answers, so a filter cannot read a hidden value one guess at a time.
func TestDelivery_FilterOnPrivateFieldAnswersLikeUnknownField(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		vault := rg.post(t, "t1", "dl_vaults", "", map[string]any{"code": "v-code"})
		rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "secret": "hush", "vault": vault})

		assert.Len(t, deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath+"?filters[title]=t", nil)), 1)

		unknown := rg.get(t, deliveryHostA, deliveryPostsPath+"?filters[nosuch]=hush", nil)
		require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
		for _, filter := range []string{"secret=hush", "secret=other", "vault=" + vault, "vault_id=" + vault} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?filters["+strings.Replace(filter, "=", "]=", 1), nil)
			assert.Equal(t, unknown.Code, rr.Code, filter)
			assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), filter)
		}
	})
}

// The status and the trash are outside the delivery grammar, so no
// parameter lifts the published default.
func TestDelivery_StatusFilterIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		rg.post(t, "t1", "dl_posts", "draft", map[string]any{"title": "draft"})

		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath, nil).Code)
		for _, q := range []string{"filters[_status]=draft", "filters[_status]=published", "filters[deleted_at]=x", "filters[created_at]=x"} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, q)
			assert.NotContains(t, rr.Body.String(), "draft\"", q)
		}
	})
}

// A relation into a type that is not marked is left out as a column and as
// a populated record when depth reaches it, and a path naming it answers what
// a path naming no relation answers.
func TestDelivery_RelationIntoUnmarkedSchemaIsLeftOut(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		vault := rg.post(t, "t1", "dl_vaults", "", map[string]any{"code": "v-code"})
		author := rg.post(t, "t1", "dl_people", "published", map[string]any{"name": "Ada"})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "author": author, "vault": vault})

		unknown := rg.get(t, deliveryHostA, deliveryPostsPath+"?populate=nosuch", nil)
		require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
		for _, q := range []string{"?populate=vault", "?populate=author,vault"} {
			for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
				rr := rg.get(t, deliveryHostA, path+q, nil)
				assert.Equal(t, http.StatusBadRequest, rr.Code, path+q)
				assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), path+q)
			}
		}
		for _, q := range []string{"", "?depth=2"} {
			for _, rr := range []*httptest.ResponseRecorder{
				rg.get(t, deliveryHostA, deliveryPostsPath+q, nil),
				rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id+q, nil),
			} {
				require.Equal(t, http.StatusOK, rr.Code, q+": "+rr.Body.String())
				assert.NotContains(t, rr.Body.String(), vault, q)
				assert.NotContains(t, rr.Body.String(), "v-code", q)
				assert.NotContains(t, rr.Body.String(), `"vault`, q)
			}
		}
		populated, _ := dataOf(t, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id+"?populate=author", nil))["author"].(map[string]any)
		require.NotNil(t, populated, "a relation into a marked type was left out")
		assert.Equal(t, "Ada", populated["name"])
	})
}

// A populated record that is not published reads as null.
func TestDelivery_PopulatedDraftReadsAsNull(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		hidden := rg.post(t, "t1", "dl_people", "draft", map[string]any{"name": "Unannounced"})
		shown := rg.post(t, "t1", "dl_people", "published", map[string]any{"name": "Ada"})
		withDraft := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "a", "author": hidden})
		withLive := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "b", "author": shown})

		rr := rg.get(t, deliveryHostA, deliveryPostsPath+"/"+withDraft+"?populate=author", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Nil(t, dataOf(t, rr)["author"])
		assert.NotContains(t, rr.Body.String(), "Unannounced")

		live, _ := dataOf(t, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+withLive+"?populate=author", nil))["author"].(map[string]any)
		require.NotNil(t, live)
		assert.Equal(t, "Ada", live["name"])
	})
}

// A request that carries a credential is refused, and the refusal is one no
// cache keeps. Served, it would be answered in the credential's tenant under
// the host's URL, which is the key a CDN stores it under.
func TestDelivery_CredentialIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})
		token, err := auth.Sign(rg.cfg.JWTSecrets[0], 3600, uuid.New(), "reader@example.com", []string{"viewer"}, "t2", 1)
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath, nil).Code)
		for name, set := range map[string]func(*http.Request){
			"bearer token":   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) },
			"API key":        func(r *http.Request) { r.Header.Set("X-API-Key", "lyeve_test_key") },
			"session cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: token}) },
		} {
			for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
				rr := rg.get(t, deliveryHostA, path, set)
				assert.Equal(t, http.StatusBadRequest, rr.Code, name+" "+path+": "+rr.Body.String())
				cc := rr.Header().Get("Cache-Control")
				assert.Contains(t, cc, "no-store", name)
				assert.NotContains(t, cc, "public", name)
				assert.Empty(t, rr.Header().Get("ETag"), name)
				assert.NotContains(t, rr.Body.String(), "pub\"", name)
			}
		}
	})
}

// Two tenants share one table and only the first marks the type. The host
// names the tenant, and nothing the request carries moves it.
func TestDelivery_TenantComesFromHostOnly(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, func(_ *config.Config, src *deliverySource) {
			src.unmarked["t2/dl_posts"] = true
		})
		one := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "one"})
		two := rg.post(t, "t2", "dl_posts", "published", map[string]any{"title": "two"})

		assert.ElementsMatch(t, []string{one}, deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath, nil)))
		header := rg.get(t, deliveryHostA, deliveryPostsPath, func(r *http.Request) { r.Header.Set("X-Tenant-ID", "t2") })
		assert.ElementsMatch(t, []string{one}, deliveredIDs(t, header))
		assert.Equal(t, http.StatusNotFound, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+two, nil).Code)

		assert.Equal(t, http.StatusBadRequest, rg.get(t, deliveryHostA, deliveryPostsPath+"?tenant=t2", nil).Code)
		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + two} {
			rr := rg.get(t, deliveryHostB, path, nil)
			assert.Equal(t, http.StatusNotFound, rr.Code, path)
			assert.NotContains(t, rr.Body.String(), "two", path)
		}
	})
}

// On an install with several tenants a host nobody registered resolves no
// tenant, and the delivery routes answer 404 for it.
func TestDelivery_UnresolvedHostAnswersNotFound(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})

		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath, nil).Code)
		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			rr := rg.get(t, "nobody.example.test", path, nil)
			assert.Equal(t, http.StatusNotFound, rr.Code, path)
			assert.Contains(t, rr.Header().Get("Cache-Control"), "no-store", path)
		}
	})
}

// A delivery answer is public with a shared lifetime, a strong ETag, no
// cookie, and a CORS answer every site shares whatever CORS_ORIGINS names.
// The two lifetime settings move only their own directives.
func TestDelivery_ResponseIsPublicWithSharedMaxAge(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, func(cfg *config.Config, _ *deliverySource) {
			cfg.CORSOrigins = []string{"https://console.example.test"}
		})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})
		origin := func(r *http.Request) { r.Header.Set("Origin", "https://site.example.test") }

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			rr := rg.get(t, deliveryHostA, path, origin)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			h := rr.Header()
			assert.Equal(t, deliveryPublicCC, h.Get("Cache-Control"), path)
			assert.Regexp(t, `^"[0-9a-f]{64}"$`, h.Get("ETag"), path)
			assert.Empty(t, h.Values("Set-Cookie"), path)
			assert.Equal(t, "*", h.Get("Access-Control-Allow-Origin"), path)
			assert.Empty(t, h.Get("Access-Control-Allow-Credentials"), path)
			assert.NotContains(t, strings.ToLower(strings.Join(h.Values("Vary"), ",")), "origin", path)
		}

		tuned := newDeliveryRig(t, fx, func(cfg *config.Config, _ *deliverySource) {
			cfg.DeliveryMaxAge = 30 * time.Second
			cfg.DeliverySharedMaxAge = 900 * time.Second
		})
		rr := tuned.get(t, deliveryHostA, deliveryPostsPath, nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, "public, max-age=30, s-maxage=900, stale-while-revalidate=60, stale-if-error=600", rr.Header().Get("Cache-Control"))
	})
}

// A cache revalidating with the ETag it holds gets 304 and no body.
func TestDelivery_IfNoneMatchAnswersNotModified(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			first := rg.get(t, deliveryHostA, path, nil)
			require.Equal(t, http.StatusOK, first.Code, first.Body.String())
			etag := first.Header().Get("ETag")
			require.NotEmpty(t, etag, path)
			again := rg.get(t, deliveryHostA, path, func(r *http.Request) { r.Header.Set("If-None-Match", etag) })
			assert.Equal(t, http.StatusNotModified, again.Code, path)
			assert.Empty(t, again.Body.String(), path)
		}
	})
}

// Localized delivery is not served yet, so locale is outside the grammar
// even with a localizer registered, and Accept-Language is not read, so the
// answer does not vary by it.
func TestDelivery_LocaleIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil, WithContentLocalizer(&localizerProvider{l: &recordingLocalizer{}}))
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub", "body": "first"})
		french := func(r *http.Request) { r.Header.Set("Accept-Language", "fr") }

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			stored := rg.get(t, deliveryHostA, path, french)
			require.Equal(t, http.StatusOK, stored.Code, stored.Body.String())
			assert.Contains(t, stored.Body.String(), "first", path)
			assert.NotContains(t, stored.Body.String(), "premier", path)
			assert.NotContains(t, strings.ToLower(strings.Join(stored.Header().Values("Vary"), ",")), "accept-language", path)

			for _, q := range []string{"?locale=fr", "?locale=not%20a%20tag"} {
				rr := rg.get(t, deliveryHostA, path+q, nil)
				assert.Equal(t, http.StatusBadRequest, rr.Code, path+q)
				assert.NotContains(t, rr.Body.String(), "premier", path+q)
			}
		}
	})
}

// A parameter outside the grammar, or one sent twice, is refused rather than
// ignored, because each would be a second URL for one body.
func TestDelivery_UnknownParameterIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})

		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath+"?limit=5&filters[title]=pub", nil).Code)
		four := deliveryPostsPath + "?filters[title]=pub&filters[body]=b&filters[id]=" + id + "&filters[rank]=1"
		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, four, nil).Code, "four valid filters are served")
		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id+"?populate=author&depth=1", nil).Code)
		for _, path := range []string{
			deliveryPostsPath + "?sort=title",
			deliveryPostsPath + "?limit=5&limit=6",
			deliveryPostsPath + "?filters[title]=a&filters[title]=b",
			deliveryPostsPath + "?filters[title]=pub&filters[body]=b&filters[id]=" + id + "&filters[rank]=1&filters[featured]=true",
			deliveryPostsPath + "?filters[title]=" + strings.Repeat("a", 256),
			deliveryPostsPath + "?populate=a,b,c,d,e,f,g,h,i",
			deliveryPostsPath + "?filters[title",
			deliveryPostsPath + "/" + id + "?limit=5",
			deliveryPostsPath + "/" + id + "?filters[title]=pub",
			deliveryPostsPath + "/" + id + "?nocache=1",
		} {
			assert.Equal(t, http.StatusBadRequest, rg.get(t, deliveryHostA, path, nil).Code, path)
		}
	})
}

// Offset, limit and depth are bounded and refused past the bound, where the
// content API clamps or takes the value.
func TestDelivery_OffsetPastCapIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})

		for _, q := range []string{"offset=10000", "offset=0", "limit=1", "limit=200", "depth=0", "depth=2"} {
			assert.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil).Code, q)
		}
		for _, q := range []string{"offset=10001", "offset=-1", "limit=0", "limit=201", "depth=3", "depth=-1", "limit=", "offset=1e3"} {
			assert.Equal(t, http.StatusBadRequest, rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil).Code, q)
		}
	})
}

// A collation that folds case would read another spelling of a type's name
// as the type, and a cache would keep the same body under two URLs.
func TestDelivery_SchemaNameSpelledOtherwiseAnswersNotFound(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, func(_ *config.Config, src *deliverySource) { src.fold = true })
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})

		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath, nil).Code)
		for _, path := range []string{"/api/v1/delivery/DL_Posts", "/api/v1/delivery/DL_POSTS/" + id} {
			assert.Equal(t, http.StatusNotFound, rg.get(t, deliveryHostA, path, nil).Code, path)
		}
	})
}

// A publish flushes the store's caches the delivery list reads through, so
// the next list serves the entry.
func TestDelivery_PublishShowsOnNextList(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		id := rg.post(t, "t1", "dl_posts", "draft", map[string]any{"title": "soon"})

		assert.Empty(t, deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath, nil)))
		assert.Equal(t, http.StatusNotFound, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id, nil).Code)

		rr := fx.send(t, fx.ctx, http.MethodPut, "/api/v1/content/dl_posts/"+id+"/publish",
			map[string]string{"schema": "dl_posts", "id": id}, nil, fx.content.Publish)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		assert.ElementsMatch(t, []string{id}, deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath, nil)))
		assert.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id, nil).Code)
	})
}

// A definition that cannot be read closes the type rather than serving it,
// and the answer says to retry and is not stored.
func TestDelivery_UnreadableDefinitionAnswersUnavailable(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		vault := rg.post(t, "t1", "dl_vaults", "", map[string]any{"code": "v-code"})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub", "vault": vault})
		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath, nil).Code)

		for _, failing := range []string{"dl_posts", "dl_vaults"} {
			rg.src.failing = map[string]bool{failing: true}
			for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
				rr := rg.get(t, deliveryHostA, path, nil)
				assert.Equal(t, http.StatusServiceUnavailable, rr.Code, failing+" "+path+": "+rr.Body.String())
				assert.Contains(t, rr.Header().Get("Cache-Control"), "no-store", failing)
				assert.NotContains(t, rr.Body.String(), "v-code", failing)
			}
		}
	})
}

// A marked type without draft and publish has no status, so delivery serves
// every entry of it, by list, by id and as a populated record.
func TestDelivery_SchemaWithoutDraftPublishServesEveryEntry(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		one := rg.post(t, "t1", "dl_tags", "", map[string]any{"label": "go"})
		two := rg.post(t, "t1", "dl_tags", "", map[string]any{"label": "sql"})
		author := rg.post(t, "t1", "dl_people", "published", map[string]any{"name": "Ada", "badge": one})

		assert.ElementsMatch(t, []string{one, two}, deliveredIDs(t, rg.get(t, deliveryHostA, "/api/v1/delivery/dl_tags", nil)))
		rr := rg.get(t, deliveryHostA, "/api/v1/delivery/dl_tags/"+two, nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, "sql", dataOf(t, rr)["label"])
		badge, _ := dataOf(t, rg.get(t, deliveryHostA, "/api/v1/delivery/dl_people/"+author+"?populate=badge", nil))["badge"].(map[string]any)
		require.NotNil(t, badge)
		assert.Equal(t, "go", badge["label"])
	})
}

// Populate follows belongs_to relations into marked types, at most two
// segments deep. A longer path, a to-many relation, a wildcard and a field
// mask are refused, by list and by id.
func TestDelivery_PopulateFollowsToOneRelationsOnly(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		tag := rg.post(t, "t1", "dl_tags", "", map[string]any{"label": "go"})
		author := rg.post(t, "t1", "dl_people", "published", map[string]any{"name": "Ada", "badge": tag})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "author": author})

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			rr := rg.get(t, deliveryHostA, path+"?populate=author.badge", nil)
			require.Equal(t, http.StatusOK, rr.Code, path+": "+rr.Body.String())
			assert.Contains(t, rr.Body.String(), `"label":"go"`, path)
			for _, q := range []string{
				"populate=author.badge.x",
				"populate=author.author.author",
				"populate=tags",
				"populate=author,tags",
				"populate=*",
				"populate=author.*",
				"populate=author%7Bname%7D",
				"populate=*%7Bname%7D",
				"populate=author.",
				"populate=nosuch",
				"populate=title",
				"populate=vault",
			} {
				rr := rg.get(t, deliveryHostA, path+"?"+q, nil)
				assert.Equal(t, http.StatusBadRequest, rr.Code, path+"?"+q+": "+rr.Body.String())
				assert.Contains(t, rr.Header().Get("Cache-Control"), "no-store", q)
			}
		}
	})
}

// depth reaches the belongs_to relations of each level and skips every
// to-many one, so a list does not pull every entry a relation points back at.
func TestDelivery_DepthSkipsToManyRelations(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		tag := rg.post(t, "t1", "dl_tags", "", map[string]any{"label": "go"})
		author := rg.post(t, "t1", "dl_people", "published", map[string]any{"name": "Ada", "badge": tag})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "author": author})
		rr := fx.send(t, fx.ctx, http.MethodPut, "/api/v1/content/dl_posts/"+id+"/relations/tags",
			map[string]string{"schema": "dl_posts", "id": id, "field": "tags"},
			map[string]any{"ids": []string{tag}}, fx.content.SetRelations)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			rr := rg.get(t, deliveryHostA, path+"?depth=2", nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var data map[string]any
			if strings.HasSuffix(path, id) {
				data = dataOf(t, rr)
			} else {
				rows := deliveredRows(t, rr)
				require.Len(t, rows, 1)
				data = rows[0]
			}
			assert.NotContains(t, data, "tags", path)
			populated, _ := data["author"].(map[string]any)
			require.NotNil(t, populated, path)
			badge, _ := populated["badge"].(map[string]any)
			require.NotNil(t, badge, path)
			assert.Equal(t, "go", badge["label"], path)
		}
	})
}

// A relation the database cannot read answers 503, never a 200 with the
// relation empty, and the answer is not stored.
func TestDelivery_RelationReadFailureAnswersUnavailable(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		author := rg.post(t, "t1", "dl_people", "published", map[string]any{"name": "Ada"})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "author": author})

		rg.pool.fail(domain.TableName("dl_people"))
		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			for _, q := range []string{"?populate=author", "?depth=1"} {
				rr := rg.get(t, deliveryHostA, path+q, nil)
				assert.Equal(t, http.StatusServiceUnavailable, rr.Code, path+q+": "+rr.Body.String())
				assert.Contains(t, rr.Header().Get("Cache-Control"), "no-store", path+q)
				assert.Empty(t, rr.Header().Get("ETag"), path+q)
			}
		}
		rg.pool.fail("")
		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id+"?populate=author", nil).Code)
	})
}

// A filter value is checked against its field's type before any query. A
// value of the wrong form, or a filter on a type whose values are not
// checked, answers what a filter on an unknown field answers, on every
// dialect. A value of the right form matches.
func TestDelivery_FilterValueIsCheckedByType(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		author := rg.post(t, "t1", "dl_people", "published", map[string]any{"name": "Ada"})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{
			"title": "t", "author": author, "rank": 7, "price": "9.50", "featured": true,
			"day": "2026-11-04", "at": "2026-11-04T10:00:00Z",
		})
		rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "other", "rank": 8, "price": "1.00", "featured": false})

		for _, q := range []string{
			"filters[id]=" + id, "filters[author_id]=" + author, "filters[rank]=7",
			"filters[price]=9.5", "filters[featured]=true", "filters[day]=2026-11-04",
			"filters[at]=2026-11-04T10:00:00Z",
		} {
			assert.Equal(t, []string{id}, deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil)), q)
		}

		unknown := rg.get(t, deliveryHostA, deliveryPostsPath+"?filters[nosuch]=x", nil)
		require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
		for _, q := range []string{
			"filters[id]=x", "filters[id]=" + strings.ToUpper(id), "filters[author_id]=x",
			"filters[rank]=x", "filters[rank]=1.5", "filters[price]=x", "filters[featured]=yes",
			"filters[day]=x", "filters[day]=2026-13-45", "filters[at]=x", "filters[extra]=x",
		} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, q+": "+rr.Body.String())
			assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), q)
		}
	})
}

// An id in any spelling but the canonical one answers 404, so one entry has
// one URL.
func TestDelivery_NonCanonicalIDAnswersNotFound(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})

		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id, nil).Code)
		for _, spelled := range []string{
			strings.ToUpper(id), "%7B" + id + "%7D", "urn:uuid:" + id, strings.ReplaceAll(id, "-", ""),
		} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"/"+spelled, nil)
			assert.Equal(t, http.StatusNotFound, rr.Code, spelled+": "+rr.Body.String())
			assert.Contains(t, rr.Header().Get("Cache-Control"), "no-store", spelled)
		}
	})
}

// The AfterResponse hook runs on the answer as it is served, so a hook never
// sees a private field.
func TestDelivery_AfterResponseHookSeesMaskedData(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "secret": "hush"})
		var mu sync.Mutex
		var seen []map[string]any
		rg.hooks.Register("dl_posts", hooks.AfterResponse, func(_ context.Context, e hooks.Event) error {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, e.Data)
			return nil
		})

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, path, nil).Code, path)
		}
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, seen, 2)
		for _, data := range seen {
			assert.Equal(t, "t", data["title"])
			assert.NotContains(t, data, "secret")
		}
	})
}

// A preflight to a delivery path is answered for any origin, CORS_ORIGINS
// or not, and allows no credential and no credential header. Every other
// path keeps the router's own answer.
func TestDelivery_PreflightAnswersAnyOrigin(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, func(cfg *config.Config, _ *deliverySource) {
			cfg.CORSOrigins = []string{"https://console.example.test"}
		})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})
		preflight := func(r *http.Request) {
			r.Method = http.MethodOptions
			r.Header.Set("Origin", "https://site.example.test")
			r.Header.Set("Access-Control-Request-Method", "GET")
			r.Header.Set("Access-Control-Request-Headers", "authorization, x-api-key, if-none-match")
		}

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			rr := rg.get(t, deliveryHostA, path, preflight)
			assert.Equal(t, http.StatusNoContent, rr.Code, path)
			h := rr.Header()
			assert.Equal(t, "*", h.Get("Access-Control-Allow-Origin"), path)
			assert.Equal(t, "GET, HEAD", h.Get("Access-Control-Allow-Methods"), path)
			assert.Equal(t, "If-None-Match, If-Modified-Since", h.Get("Access-Control-Allow-Headers"), path)
			assert.NotEmpty(t, h.Get("Access-Control-Max-Age"), path)
			assert.Empty(t, h.Get("Access-Control-Allow-Credentials"), path)
			assert.NotContains(t, strings.ToLower(strings.Join(h.Values("Vary"), ",")), "origin", path)
		}

		other := rg.get(t, deliveryHostA, "/api/v1/content/dl_posts", preflight)
		assert.Equal(t, http.StatusNoContent, other.Code)
		assert.Empty(t, other.Header().Get("Access-Control-Allow-Origin"), "an origin outside CORS_ORIGINS is not allowed elsewhere")
	})
}

// A delivery answer varies on no credential header, because a credentialed
// request never gets one, and a shared cache that refuses to store a
// response varying on them would not keep it.
func TestDelivery_ResponseVariesOnNoCredential(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, func(cfg *config.Config, _ *deliverySource) {
			cfg.CORSOrigins = []string{"https://console.example.test"}
		})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			rr := rg.get(t, deliveryHostA, path, func(r *http.Request) { r.Header.Set("Origin", "https://site.example.test") })
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			vary := strings.ToLower(strings.Join(rr.Header().Values("Vary"), ","))
			for _, name := range []string{"authorization", "x-api-key", "origin"} {
				assert.NotContains(t, vary, name, path)
			}
		}
	})
}

// The last page the grammar admits is an empty list, not an error.
func TestDelivery_OffsetAtCapServesEmptyList(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "pub"})

		rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?offset=10000", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.JSONEq(t, `[]`, rr.Body.String())
	})
}

// stripRequestID drops the request id from an error body, which differs on
// every request, so two refusals compare by what they say.
func stripRequestID(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &m), body)
	delete(m, "request_id")
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return string(b)
}
