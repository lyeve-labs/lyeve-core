package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// withRoles returns a request carrying JWT claims for the given roles.
func withRoles(roles ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/content/posts", nil)
	claims := &auth.Claims{UserID: "00000000-0000-0000-0000-000000000001", Roles: roles}
	return r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
}

// asSuperAdmin returns ctx carrying a super_admin session. Handler tests whose
// subject is not authorization act as this caller, because the content
// handler refuses a request that carries no caller at all.
func asSuperAdmin(ctx context.Context) context.Context {
	claims := &auth.Claims{UserID: "00000000-0000-0000-0000-000000000001", Roles: []string{"super_admin"}}
	return context.WithValue(ctx, auth.ClaimsKey, claims)
}

// contentReq is chiCtx for a content handler test, with the request made by a
// super_admin.
func contentReq(r *http.Request, params map[string]string) *http.Request {
	return chiCtx(r.WithContext(asSuperAdmin(r.Context())), params)
}

// fakeChecker stands in for the plugin that owns the rules.
type fakeChecker struct {
	allow bool
	mask  []string
	err   error
}

func (c fakeChecker) Allowed(context.Context, []string, string, string) (bool, error) {
	return c.allow, c.err
}

func (c fakeChecker) FieldMask(context.Context, []string, string) ([]string, error) {
	return c.mask, c.err
}

// checkerOnly implements PermissionChecker and not FieldMasker, which is what
// a rule engine with no field rules looks like.
type checkerOnly struct{ allow bool }

func (c checkerOnly) Allowed(context.Context, []string, string, string) (bool, error) {
	return c.allow, nil
}

type fakeProvider struct{ c core.PermissionChecker }

func (p fakeProvider) PermissionChecker() core.PermissionChecker { return p.c }

// A build with no plugin supplying a rule engine is the ordinary shape of the kernel on its own, not an error path. It has to be neither an open database
// nor a closed one: super_admin reaches everything, because only a super_admin
// can write the first rule, and every other role is refused a schema.
//
// A plugin's resources are the exception for admin: a kind registered with
// AdminByDefault admits admin while no rule names it, the way a plugin that
// owns such a kind registers it from init.
func TestContentHandler_Authorization_NoRuleEngine(t *testing.T) {
	for _, general := range []string{"reports", "vaults"} {
		core.RegisterPermissionKind(core.PermissionKind{
			General:        general,
			Prefix:         strings.TrimSuffix(general, "s") + ":",
			ValidSlug:      func(slug string) bool { return slug != "" },
			Actions:        []string{"export"},
			AdminByDefault: true,
		})
	}
	h := &ContentHandler{hooks: hooks.NewRegistry()}

	cases := []struct {
		name     string
		roles    []string
		resource string
		want     bool
	}{
		{"super admin reaches a schema", []string{"super_admin"}, "posts", true},
		{"admin does not reach a schema", []string{"admin"}, "posts", false},
		{"editor does not reach a schema", []string{"editor"}, "posts", false},
		{"no role reaches a schema", []string{}, "posts", false},
		{"admin still reaches every report", []string{"admin"}, "reports", true},
		{"admin still reaches one report", []string{"admin"}, "report:weekly", true},
		{"admin still reaches a vault", []string{"admin"}, "vault:main", true},
		{"editor does not reach a report", []string{"editor"}, "reports", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			got := h.checkPermission(rr, withRoles(tc.roles...), tc.resource, "create")
			if got != tc.want {
				t.Fatalf("allowed = %v, want %v (status %d, body %s)", got, tc.want, rr.Code, rr.Body.String())
			}
			if !tc.want && rr.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rr.Code)
			}
		})
	}
}

// The kernel's own checker has no rules, so it has no field masks either. A
// read on a build without the plugin serves every field it stored.
func TestContentHandler_MaskFor_NoRuleEngine(t *testing.T) {
	h := &ContentHandler{hooks: hooks.NewRegistry()}
	rr := httptest.NewRecorder()
	mask, ok := h.maskFor(rr, withRoles("editor"), "posts")
	if !ok {
		t.Fatalf("mask refused: %d %s", rr.Code, rr.Body.String())
	}
	data := map[string]any{"title": "hello", "secret": "shh"}
	got := mask(data)
	if len(got) != 2 {
		t.Errorf("fields = %d, want 2: %v", len(got), got)
	}
}

// A supplied rule engine decides, and the kernel's default does not apply.
func TestContentHandler_Authorization_PluginChecker(t *testing.T) {
	t.Run("a grant the kernel would refuse passes", func(t *testing.T) {
		h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: true}}}
		rr := httptest.NewRecorder()
		if !h.checkPermission(rr, withRoles("editor"), "posts", "create") {
			t.Fatalf("granted editor refused: %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("a refusal the kernel would grant is refused", func(t *testing.T) {
		h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: false}}}
		rr := httptest.NewRecorder()
		if h.checkPermission(rr, withRoles("super_admin"), "posts", "create") {
			t.Fatal("super_admin allowed although the rule engine refused")
		}
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rr.Code)
		}
	})

	t.Run("a provider holding no checker falls back to the kernel", func(t *testing.T) {
		h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{nil}}
		rr := httptest.NewRecorder()
		if !h.checkPermission(rr, withRoles("super_admin"), "posts", "create") {
			t.Fatalf("super_admin refused after the rule engine stopped: %d", rr.Code)
		}
		rr = httptest.NewRecorder()
		if h.checkPermission(rr, withRoles("editor"), "posts", "create") {
			t.Fatal("editor allowed after the rule engine stopped")
		}
	})
}

// A rule store that cannot be read is a database failure, not a refusal, so it
// answers 503. A 403 would tell an operator their roles were wrong when the
// database was down.
func TestContentHandler_Authorization_StoreFailureIs503(t *testing.T) {
	boom := errors.New("connection refused")
	h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{err: boom}}}

	rr := httptest.NewRecorder()
	if h.checkPermission(rr, withRoles("editor"), "posts", "create") {
		t.Fatal("allowed although the rule store failed")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}

	rr = httptest.NewRecorder()
	if _, ok := h.maskFor(rr, withRoles("editor"), "posts"); ok {
		t.Fatal("mask resolved although the rule store failed")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("mask status = %d, want 503", rr.Code)
	}
}

// Masking is resolved through the checker the plugin supplies. A checker that
// declares no field rules hides nothing.
func TestContentHandler_MaskFor_PluginChecker(t *testing.T) {
	t.Run("a masked field is removed", func(t *testing.T) {
		h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{mask: []string{"secret"}}}}
		rr := httptest.NewRecorder()
		mask, ok := h.maskFor(rr, withRoles("editor"), "posts")
		if !ok {
			t.Fatalf("mask refused: %d", rr.Code)
		}
		got := mask(map[string]any{"title": "hello", "secret": "shh"})
		if _, present := got["secret"]; present {
			t.Error("masked field served")
		}
		if got["title"] != "hello" {
			t.Error("unmasked field lost")
		}
	})

	t.Run("a checker with no field rules hides nothing", func(t *testing.T) {
		h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{checkerOnly{allow: true}}}
		rr := httptest.NewRecorder()
		mask, ok := h.maskFor(rr, withRoles("editor"), "posts")
		if !ok {
			t.Fatalf("mask refused: %d", rr.Code)
		}
		got := mask(map[string]any{"title": "hello", "secret": "shh"})
		if len(got) != 2 {
			t.Errorf("fields = %d, want 2", len(got))
		}
	})
}

// withKey returns a request carrying the claims the API key middleware
// writes, under the key it writes them to.
func withKey(claims *core.AuthClaims) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	claims.IsAPIKey = true
	return r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims))
}

// An API key is judged by the same rules as a session holding its roles, and
// a key limited to a list of schemas is refused every other schema. The
// handler reads a key request's own claims, so the rules apply to it as they
// do to a session.
func TestContentHandler_Authorization_APIKey(t *testing.T) {
	boom := errors.New("connection refused")
	cases := []struct {
		name    string
		checker core.PermissionChecker
		schemas []string
		schema  string
		status  int
	}{
		{"a role the rules refuse is refused", fakeChecker{allow: false}, nil, "posts", http.StatusForbidden},
		{"a role the rules allow is allowed", fakeChecker{allow: true}, nil, "posts", http.StatusOK},
		{"an empty list reaches what the rules allow", fakeChecker{allow: true}, []string{}, "pages", http.StatusOK},
		{"a listed schema is allowed", fakeChecker{allow: true}, []string{"posts"}, "posts", http.StatusOK},
		{"an unlisted schema is refused although the rules allow it", fakeChecker{allow: true}, []string{"posts"}, "pages", http.StatusForbidden},
		{"a listed schema the rules refuse is refused", fakeChecker{allow: false}, []string{"posts"}, "posts", http.StatusForbidden},
		{"a rule store failure is 503", fakeChecker{err: boom}, nil, "posts", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{tc.checker}}
			rr := httptest.NewRecorder()
			r := withKey(&core.AuthClaims{UserID: "key-1", Roles: []string{"reader"}, Scopes: []string{"content:read"}, Schemas: tc.schemas})
			got := h.checkPermission(rr, r, tc.schema, "read")
			if got != (tc.status == http.StatusOK) {
				t.Fatalf("allowed = %v, want status %d (got %d %s)", got, tc.status, rr.Code, rr.Body.String())
			}
			if !got && rr.Code != tc.status {
				t.Errorf("status = %d, want %d", rr.Code, tc.status)
			}
		})
	}

	t.Run("a schema list binds a super_admin key too", func(t *testing.T) {
		h := &ContentHandler{hooks: hooks.NewRegistry()}
		rr := httptest.NewRecorder()
		r := withKey(&core.AuthClaims{UserID: "key-1", Roles: []string{"super_admin"}, Schemas: []string{"posts"}})
		if h.checkPermission(rr, r, "pages", "read") {
			t.Fatal("super_admin key reached a schema outside its list")
		}
		if !h.checkPermission(httptest.NewRecorder(), r, "posts", "read") {
			t.Fatal("super_admin key refused a schema on its list")
		}
	})
}

// A key's field mask comes from its roles, as a session's does. With no
// caller the mask is refused rather than resolved to "hide nothing".
func TestContentHandler_MaskFor_APIKey(t *testing.T) {
	h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: true, mask: []string{"internal"}}}}

	rr := httptest.NewRecorder()
	mask, ok := h.maskFor(rr, withKey(&core.AuthClaims{UserID: "key-1", Roles: []string{"reader"}}), "posts")
	if !ok {
		t.Fatalf("mask refused: %d %s", rr.Code, rr.Body.String())
	}
	got := mask(map[string]any{"title": "hello", "internal": "shh"})
	if _, present := got["internal"]; present {
		t.Error("masked field served to an API key")
	}
	if got["title"] != "hello" {
		t.Error("unmasked field lost")
	}

	rr = httptest.NewRecorder()
	if _, ok := h.maskFor(rr, httptest.NewRequest(http.MethodGet, "/", nil), "posts"); ok {
		t.Fatal("mask resolved for a request with no caller")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// requestClaims finds the caller whichever middleware recorded it, and
// prefers the session claims requireRole reads when both are present, so the
// role gate and the rule check judge one identity.
func TestRequestClaims_EachCredential(t *testing.T) {
	session := &auth.Claims{UserID: "user-1", Roles: []string{"editor"}}
	key := &core.AuthClaims{UserID: "key-1", Roles: []string{"reader"}, IsAPIKey: true}

	cases := []struct {
		name  string
		ctx   func(context.Context) context.Context
		wants string
	}{
		{"no caller", func(c context.Context) context.Context { return c }, ""},
		{"session only, as a trusted issuer records it", func(c context.Context) context.Context {
			return context.WithValue(c, auth.ClaimsKey, session)
		}, "user-1"},
		{"api key only", func(c context.Context) context.Context {
			return context.WithValue(c, core.ClaimsKey, key)
		}, "key-1"},
		{"both, session first", func(c context.Context) context.Context {
			return context.WithValue(context.WithValue(c, core.ClaimsKey, key), auth.ClaimsKey, session)
		}, "user-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			got := requestClaims(r.WithContext(tc.ctx(r.Context())))
			if tc.wants == "" {
				if got != nil {
					t.Fatalf("claims = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.UserID != tc.wants {
				t.Fatalf("claims = %+v, want subject %q", got, tc.wants)
			}
		})
	}
}

// The relation routes run the same gate as every other content route, so a key
// limited to one schema cannot read or rewrite the links of another.
func TestContentHandler_Relations_APIKeyOffItsList(t *testing.T) {
	h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: true}}}
	params := map[string]string{"schema": "pages", "id": "00000000-0000-0000-0000-000000000002", "field": "tags"}
	key := &core.AuthClaims{UserID: "key-1", Roles: []string{"editor"}, Schemas: []string{"posts"}}

	for name, serve := range map[string]http.HandlerFunc{"list": h.ListRelations, "set": h.SetRelations} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			serve(rr, chiCtx(withKey(key), params))
			if rr.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// The scope gate reads the schema a path names, and a relation or a populated
// field reaches a second one. authorize decides every schema a content
// handler touches, so it holds a key scoped to one schema by name to that
// schema, while a key scoped to the bare resource reaches them all.
func TestContentHandler_Authorize_HoldsAKeyToItsNamedSchemas(t *testing.T) {
	h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: true}}}
	cases := []struct {
		name   string
		claims *core.AuthClaims
		key    bool
		schema string
		action string
		want   error
	}{
		{"a named schema is reached", &core.AuthClaims{Roles: []string{"reader"}, Scopes: []string{"content.posts:read"}}, true, "posts", "read", nil},
		{"another schema is refused", &core.AuthClaims{Roles: []string{"reader"}, Scopes: []string{"content.posts:read"}}, true, "authors", "read", errScopeDenied},
		{"an action the scope lacks is refused", &core.AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content.posts:read"}}, true, "posts", "update", errScopeDenied},
		{"the bare resource reaches every schema", &core.AuthClaims{Roles: []string{"reader"}, Scopes: []string{"content:read"}}, true, "authors", "read", nil},
		{"write covers an update", &core.AuthClaims{Roles: []string{"editor"}, Scopes: []string{"content:write"}}, true, "authors", "update", nil},
		{"a key with an admin role answers to its role", &core.AuthClaims{Roles: []string{"admin"}, Scopes: []string{"content.posts:read"}}, true, "authors", "read", nil},
		{"a session carries no scope and is not held to one", &core.AuthClaims{Roles: []string{"reader"}}, false, "authors", "read", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.claims.UserID = "caller"
			r := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
			if tc.key {
				r = withKey(tc.claims)
			} else {
				r = r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, tc.claims))
			}
			err := h.authorize(r, tc.schema, tc.action)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}

// streamWriter is a ResponseWriter the stream can flush to while the test
// reads it. flushed signals each flush, so the test knows when the stream has
// subscribed and when an event has gone out.
type streamWriter struct {
	mu      sync.Mutex
	header  http.Header
	body    strings.Builder
	flushed chan struct{}
}

func (s *streamWriter) Header() http.Header { return s.header }
func (s *streamWriter) WriteHeader(int)     {}
func (s *streamWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(b)
}
func (s *streamWriter) Flush() { s.flushed <- struct{}{} }
func (s *streamWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}

// A change event carries the entry, so the stream applies the caller's field
// mask to it as a read does.
func TestContentHandler_Stream_AppliesTheMask(t *testing.T) {
	reg := hooks.NewRegistry()
	h := &ContentHandler{hooks: reg, perms: fakeProvider{fakeChecker{allow: true, mask: []string{"internal"}}}}

	ctx, cancel := context.WithCancel(context.Background())
	r := withKey(&core.AuthClaims{UserID: "key-1", Roles: []string{"reader"}, Scopes: []string{"content:read"}})
	r = chiCtx(r.WithContext(context.WithValue(ctx, core.ClaimsKey, core.GetClaims(r.Context()))), map[string]string{"schema": "posts"})
	w := &streamWriter{header: http.Header{}, flushed: make(chan struct{}, 4)}

	done := make(chan struct{})
	go func() {
		h.Stream(w, r)
		close(done)
	}()
	<-w.flushed // subscribed

	require.NoError(t, reg.Run(context.Background(), hooks.Event{
		Type: hooks.AfterCreate, Schema: "posts",
		Data: map[string]any{"title": "hello", "internal": "shh"},
	}))
	<-w.flushed // event written
	cancel()
	<-done

	out := w.String()
	if !strings.Contains(out, `"title":"hello"`) {
		t.Fatalf("event not streamed: %q", out)
	}
	if strings.Contains(out, "internal") {
		t.Errorf("masked field streamed: %q", out)
	}
}
