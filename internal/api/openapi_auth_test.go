package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// groupProbeRoutes declares one route in each group on each router, so the
// roles the document gives a declared route are held to the gate the router
// mounts for its group, as an engine route's are.
func groupProbeRoutes() []core.RouteDecl {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]any{})
	})
	var out []core.RouteDecl
	for _, prefix := range []string{"/api/admin", "/api/v1"} {
		for name, group := range map[string]core.RouteGroup{
			"public": core.GroupPublic, "signed-in": core.GroupAuth,
			"admin": core.GroupAdmin, "super-admin": core.GroupSuperAdmin,
		} {
			out = append(out, core.RouteDecl{Method: http.MethodGet, Pattern: prefix + "/doc-probe/" + name, Group: group, Handler: ok})
		}
	}
	return out
}

// gateRefusal reads the engine's own gates from an answer: 401 when the route
// needs a credential and the request carried none, 403 when the caller's
// roles fall short. Any other answer, a handler's own refusal included, is 0.
// No handler reuses either message, so the message names the gate.
func gateRefusal(status int, body []byte) int {
	var envelope struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	switch {
	case status == http.StatusUnauthorized && envelope.Error == "authentication required":
		return status
	case status == http.StatusForbidden && envelope.Error == "insufficient permissions":
		return status
	}
	return 0
}

// probeRoles is every role the document names, and one it never does, which
// stands for a signed-in caller no gate admits by role.
func probeRoles(t *testing.T, doc documentedPaths) []string {
	t.Helper()
	const unnamed = "viewer"
	named := map[string]bool{}
	for _, item := range doc.Paths {
		for _, op := range item {
			for _, role := range op.Roles {
				named[role] = true
			}
		}
	}
	require.False(t, named[unnamed], "the document names %q, so it cannot stand for a role no gate admits", unnamed)
	named[unnamed] = true
	return slices.Sorted(maps.Keys(named))
}

// The router says what a route requires and the document has to say the same.
// Nothing reads a gate back from the router's tree, where requireRole's roles
// sit in a closure, so every operation of the served document, of every
// method, is requested: once with no credential and once as each role. A
// refusal by the authentication gate requires the operation to ask for a
// credential, and an operation that asks for one must not serve a request
// without it. When the role gate refuses any role, the operation's x-roles
// must be exactly the roles it admits, and an operation that admits every
// role must name none.
//
// A request refused by a gate never reaches its handler. A write that is
// admitted does, without a body, so each write is made as a new account: the
// logout it may reach ends only that account's sessions.
func TestOpenAPI_AuthMatchesWhatTheRoutesEnforce(t *testing.T) {
	forEachProbeDialect(t, probeDocumentedAuth)
}

func probeDocumentedAuth(t *testing.T, pool db.DB) {
	p := newDocumentProbe(t, pool, WithOwnerRoutes("doc-probe", groupProbeRoutes()))
	roles := probeRoles(t, p.doc)
	readers := map[string]string{}
	for _, role := range roles {
		readers[role] = p.session(t, role)
	}

	probed := 0
	for _, path := range slices.Sorted(maps.Keys(p.doc.Paths)) {
		for _, method := range slices.Sorted(maps.Keys(p.doc.Paths[path])) {
			op := p.doc.Paths[path][method]
			verb := strings.ToUpper(method)
			server := p.serverFor(method, path)
			if server == "" {
				t.Errorf("the document lists %s %s and neither router serves it", verb, path)
				continue
			}
			probed++
			target := p.servers[server].URL + fillPathParams(path, p.values)

			status, _, body := sendProbe(t, verb, target, "", "", "")
			switch {
			case gateRefusal(status, body) == http.StatusUnauthorized && len(op.Security) == 0:
				t.Errorf("%s %s refuses a request without a credential, and the document asks for none", verb, path)
			case status != http.StatusUnauthorized && len(op.Security) > 0:
				t.Errorf("%s %s answered %d without a credential, and the document asks for one", verb, path, status)
			}

			var admitted []string
			refused := false
			for _, role := range roles {
				token := readers[role]
				if method != "get" {
					token = p.session(t, role)
				}
				status, _, body := sendProbe(t, verb, target, token, "", "")
				switch gateRefusal(status, body) {
				case http.StatusUnauthorized:
					t.Errorf("%s %s refused a %s session as if it carried no credential", verb, path, role)
				case http.StatusForbidden:
					refused = true
				default:
					admitted = append(admitted, role)
				}
			}
			documented := slices.Sorted(slices.Values(op.Roles))
			switch {
			case refused && len(documented) == 0:
				t.Errorf("%s %s admits only %v, and the document names no role", verb, path, admitted)
			case refused && !slices.Equal(documented, admitted):
				t.Errorf("%s %s admits %v, and the document names %v", verb, path, admitted, documented)
			case !refused && len(documented) > 0:
				t.Errorf("%s %s admits every role, and the document names %v", verb, path, documented)
			}
		}
	}
	require.Positive(t, probed, "no operation was requested")
	t.Logf("requested %d operations of the served document as %d roles and without a credential", probed, len(roles))
}

// The metrics route serves a scraper that presents the operator's metrics
// token and a super admin's session, and its entry offers both.
func TestOpenAPI_MetricsNamesItsTokenAndRole(t *testing.T) {
	doc := buildOpenAPIDoc(nil, nil, openAPIOptions{})
	op := documentedPathsOf(t, doc).Paths["/api/admin/metrics"]["get"]

	var schemes []string
	for _, alternative := range op.Security {
		schemes = append(schemes, slices.Collect(maps.Keys(alternative))...)
	}
	assert.ElementsMatch(t, []string{"metricsToken", "bearerAuth", "cookieAuth"}, schemes)
	assert.Equal(t, []string{"super_admin"}, op.Roles, "a session reaches the route as a super admin")

	scheme, ok := doc.Components.SecuritySchemes["metricsToken"].(map[string]any)
	require.True(t, ok, "the document defines no metricsToken scheme")
	assert.Equal(t, "http", scheme["type"])
	assert.Equal(t, "bearer", scheme["scheme"])
	assert.Contains(t, scheme["description"], "METRICS_TOKEN")
}

// The schema export is a read for an admin and the import runs DDL for a
// super admin alone, and the import's entry documents the body it reads: a
// bundle or another system's definitions, in YAML or JSON, under every media
// type the router admits on that path.
func TestOpenAPI_SchemaTransferNamesItsRolesAndBody(t *testing.T) {
	doc := documentedPathsOf(t, buildOpenAPIDoc(nil, nil, openAPIOptions{}))

	export := doc.Paths["/api/admin/schemas/export"]["get"]
	assert.ElementsMatch(t, []string{"admin", "super_admin"}, export.Roles)

	imp := doc.Paths["/api/admin/schemas/import"]["post"]
	assert.Equal(t, []string{"super_admin"}, imp.Roles)
	require.NotNil(t, imp.RequestBody, "the import documents no request body")
	assert.True(t, imp.RequestBody.Required, "an empty body is refused")
	assert.ElementsMatch(t,
		[]string{"application/json", "application/yaml", "application/x-yaml", "text/yaml"},
		slices.Collect(maps.Keys(imp.RequestBody.Content)))
}

// Each media type the import declares is one the router admits and the
// handler reads: the export's own bundle, sent back under every declared type,
// is planned. A type it does not declare is refused before the handler.
func TestOpenAPI_SchemaImportReadsEveryDeclaredMediaType(t *testing.T) {
	forEachProbeDialect(t, probeSchemaImportBody)
}

func probeSchemaImportBody(t *testing.T, pool db.DB) {
	p := newDocumentProbe(t, pool)
	admin := p.servers["admin"].URL

	status, _, bundle := sendProbe(t, http.MethodGet, admin+"/api/admin/schemas/export", p.token, "", "")
	require.Equal(t, http.StatusOK, status, string(bundle))

	imp := p.doc.Paths["/api/admin/schemas/import"]["post"]
	require.NotNil(t, imp.RequestBody, "the import documents no request body")
	require.NotEmpty(t, imp.RequestBody.Content)
	for mediaType := range imp.RequestBody.Content {
		status, _, body := sendTypedProbe(t, http.MethodPost, admin+"/api/admin/schemas/import", p.token, mediaType, string(bundle))
		assert.Equal(t, http.StatusOK, status, "%s: %s", mediaType, body)
		var answer struct {
			Plan    json.RawMessage `json:"plan"`
			Applied bool            `json:"applied"`
		}
		require.NoError(t, json.Unmarshal(body, &answer), "%s: %s", mediaType, body)
		assert.NotEmpty(t, answer.Plan, "%s: the import planned nothing", mediaType)
		assert.False(t, answer.Applied, "%s: a dry run applied the bundle", mediaType)
	}

	status, _, body := sendTypedProbe(t, http.MethodPost, admin+"/api/admin/schemas/import", p.token, "text/plain", string(bundle))
	assert.Equal(t, http.StatusUnsupportedMediaType, status, string(body))
}
