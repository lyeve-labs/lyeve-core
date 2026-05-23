package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// echoBody answers 200 with what the handler read, so a test can tell a
// request that reached it with its body intact from one the chain refused.
func echoBody(field string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var got string
		if field != "" {
			got = r.FormValue(field)
		} else {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read", http.StatusBadRequest)
				return
			}
			got = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"got": got})
	}
}

// identityRoutes declares a SAML assertion consumer and a SCIM user
// collection the way their plugins do, each naming the media type its
// protocol posts, beside an admin route that names none.
func identityRoutes() []plugin.PluginRoutes {
	return []plugin.PluginRoutes{{
		Name: "identity",
		Routes: []plugin.RouteDecl{
			{Method: http.MethodPost, Pattern: "/api/admin/auth/saml/{provider}/acs", Group: plugin.GroupPublic,
				Handler: echoBody("SAMLResponse"), MediaTypes: []string{"application/x-www-form-urlencoded"}},
			{Method: http.MethodPost, Pattern: "/api/admin/auth/saml/{provider}/logout", Group: plugin.GroupPublic,
				Handler: echoBody("SAMLResponse")},
			{Method: http.MethodPost, Pattern: "/api/v1/scim/v2/Users", Group: plugin.GroupPublic,
				Handler: echoBody(""), MediaTypes: []string{"application/scim+json"}},
			{Method: http.MethodPatch, Pattern: "/api/v1/scim/v2/Users/{id}", Group: plugin.GroupPublic,
				Handler: echoBody(""), MediaTypes: []string{"application/scim+json"}},
			{Method: http.MethodPut, Pattern: "/api/v1/scim/v2/Users/{id}", Group: plugin.GroupPublic,
				Handler: echoBody("")},
			// A pattern that overlaps an engine route: the engine's literal
			// route outranks it, so the declaration must not reach logout.
			{Method: http.MethodPost, Pattern: "/api/admin/auth/{provider}", Group: plugin.GroupPublic,
				Handler: echoBody(""), MediaTypes: []string{"application/x-www-form-urlencoded"}},
			// Shadowed outright: the engine serves this pattern itself.
			{Method: http.MethodPost, Pattern: "/api/admin/auth/refresh", Group: plugin.GroupPublic,
				Handler: echoBody(""), MediaTypes: []string{"application/x-www-form-urlencoded"}},
		},
	}}
}

// A protocol that fixes its own media type reaches its handler through the
// whole router chain, and every route that did not ask for that media type
// still answers 415.
func TestRouters_DeclaredMediaTypesReachTheHandler(t *testing.T) {
	t.Parallel()
	routes := identityRoutes()
	admin, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	apiRouter, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}
	stateless := NewStatelessRouter(testConfig(), NewStatelessMode(nil), WithLifetime(testLifetime(t)), WithPluginRoutes(routes))

	form := url.Values{"SAMLResponse": {"PHNhbWxwOlJlc3BvbnNlLz4="}, "RelayState": {"r"}}.Encode()
	user := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"ada"}`
	const formType = "application/x-www-form-urlencoded"
	const scimType = "application/scim+json; charset=utf-8"

	cases := []struct {
		name, router, method, path, ct, body string
		want                                 int
		wantGot                              string
	}{
		{"saml form post", "admin", http.MethodPost, "/api/admin/auth/saml/okta/acs", formType, form, http.StatusOK, "PHNhbWxwOlJlc3BvbnNlLz4="},
		{"saml form post stateless", "stateless", http.MethodPost, "/api/admin/auth/saml/okta/acs", formType, form, http.StatusOK, "PHNhbWxwOlJlc3BvbnNlLz4="},
		{"form on an undeclared sibling", "admin", http.MethodPost, "/api/admin/auth/saml/okta/logout", formType, form, http.StatusUnsupportedMediaType, ""},
		{"form on an engine route", "admin", http.MethodPost, "/api/admin/auth/login", formType, "email=a&password=b", http.StatusUnsupportedMediaType, ""},
		{"text on the consumer", "admin", http.MethodPost, "/api/admin/auth/saml/okta/acs", "text/plain", form, http.StatusUnsupportedMediaType, ""},
		{"scim create", "api", http.MethodPost, "/api/v1/scim/v2/Users", scimType, user, http.StatusOK, user},
		{"scim create stateless", "stateless", http.MethodPost, "/api/v1/scim/v2/Users", scimType, user, http.StatusOK, user},
		{"scim patch", "api", http.MethodPatch, "/api/v1/scim/v2/Users/42", scimType, user, http.StatusOK, user},
		{"scim on an undeclared method", "api", http.MethodPut, "/api/v1/scim/v2/Users/42", scimType, user, http.StatusUnsupportedMediaType, ""},
		{"scim media type on content", "api", http.MethodPost, "/api/v1/content/posts", scimType, user, http.StatusUnsupportedMediaType, ""},
		{"form on logout beside an overlapping declaration", "admin", http.MethodPost, "/api/admin/auth/logout", formType, form, http.StatusUnsupportedMediaType, ""},
		{"form on the overlapping declaration itself", "admin", http.MethodPost, "/api/admin/auth/okta", formType, "x=1", http.StatusOK, "x=1"},
		{"form on a shadowed declaration", "admin", http.MethodPost, "/api/admin/auth/refresh", formType, form, http.StatusUnsupportedMediaType, ""},
		{"encoded separator shaped like the consumer", "admin", http.MethodPost, "/api/admin/auth%2Fsaml/okta/acs", formType, form, http.StatusUnsupportedMediaType, ""},
		{"encoded separator shaped like a scim write", "api", http.MethodPatch, "/api/v1/scim%2Fv2/Users/42", scimType, user, http.StatusUnsupportedMediaType, ""},
		{"form on the scim route", "api", http.MethodPost, "/api/v1/scim/v2/Users", formType, form, http.StatusUnsupportedMediaType, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]http.Handler{"admin": admin, "api": apiRouter, "stateless": stateless}[tc.router]
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ct)
			// An identity provider's form post arrives cross site.
			req.Header.Set("Origin", "https://idp.example.com")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			var out map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out["got"] != tc.wantGot {
				t.Fatalf("handler read %q, want %q", out["got"], tc.wantGot)
			}
		})
	}
}
