package api

import (
	"net/http"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/schemaimport"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// Handler

// openAPIScope is which half of the document a request is served.
type openAPIScope int

const (
	// openAPIByRole serves the whole document to a super admin and the
	// public half to anyone else the route admits. It serves a reader that
	// names no half.
	openAPIByRole openAPIScope = iota
	// openAPIPublic is every route outside /api/admin: the content API,
	// auth, the probes and the plugin routes a client calls.
	openAPIPublic
	// openAPIAdmin is the /api/admin routes, for a super admin only.
	openAPIAdmin
)

// adminPathPrefix separates the two halves.
const adminPathPrefix = "/api/admin"

// prometheusProtobufMediaType is the format the metrics route answers a
// scraper whose Accept header names it. The Prometheus library negotiates it,
// and every other request gets the text format.
const prometheusProtobufMediaType = "application/vnd.google.protobuf; proto=io.prometheus.client.MetricFamily; encoding=delimited"

// metricsTokenScheme describes the metrics token to a client: the static
// bearer credential a scraper presents in place of an account.
var metricsTokenScheme = map[string]any{
	"type":        "http",
	"scheme":      "bearer",
	"description": "The token the operator set in METRICS_TOKEN, sent as Authorization: Bearer <token>. Only GET /api/admin/metrics reads it.",
}

// openAPIHandler returns the OpenAPI 3.1 specification for the CMS API, or
// the half of it the scope names.
//
// The names are read on every request, so the document reflects what the
// install can serve right now. An install with no schema engine lists no
// content paths, which is honest: there are none to call.
func openAPIHandler(schemas core.SchemaSource, plugins []plugin.PluginRoutes, docs core.EndpointDocumenter, scope openAPIScope, opts openAPIOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var names []string
		if defs, err := schemas.List(r.Context()); err == nil {
			for _, s := range defs {
				names = append(names, s.Name)
			}
		}
		doc := buildOpenAPIDoc(names, plugins, opts)
		if docs != nil {
			eps, _ := docs.DocumentedEndpoints(r.Context()) // a documenter logs its own failure and lists what it has
			documentEndpoints(doc.Paths, eps)
		}
		filename := "cms-api.json"
		switch scope {
		case openAPIPublic:
			doc = openAPIHalf(doc, false)
			filename = "cms-api-public.json"
		case openAPIAdmin:
			doc = openAPIHalf(doc, true)
			filename = "cms-api-admin.json"
		case openAPIByRole:
			if !callerHasRole(r, "super_admin") {
				doc = openAPIHalf(doc, false)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "inline; filename=\""+filename+"\"")
		_ = jsonpool.WriteJSON(w, doc) // err suppressed: response write to client
	}
}

// openAPIHalf keeps the admin paths or everything else. The admin half keeps
// the admin server alone. The public half keeps both, since the auth and
// probe routes a client uses are on each.
func openAPIHalf(doc openAPIDoc, admin bool) openAPIDoc {
	kept := make(map[string]openAPIPath, len(doc.Paths))
	for p, item := range doc.Paths {
		if isAdminPath(p) == admin {
			kept[p] = item
		}
	}
	doc.Paths = kept
	if admin {
		doc.Info.Title = "LyEve Admin API"
		doc.Info.Description = "The /api/admin routes: cookie or bearer auth, and a role on every one. An admin token reaches the operations that name the grant it holds in x-admin-grant. Served to super admins only."
	} else {
		doc.Info.Title = "LyEve API"
		doc.Info.Description = "The content API, auth, and every route outside /api/admin."
	}
	return doc
}

func isAdminPath(p string) bool {
	return p == adminPathPrefix || strings.HasPrefix(p, adminPathPrefix+"/")
}

// callerHasRole reads the caller's role the way requireRole does, from the
// session's claims or else from an API key's.
func callerHasRole(r *http.Request, role string) bool {
	if c := claimsFromCtx(r); c != nil {
		return c.HasRole(role)
	}
	if ac := core.GetClaims(r.Context()); ac != nil {
		for _, rl := range ac.Roles {
			if rl == role {
				return true
			}
		}
	}
	return false
}

// buildOpenAPIDoc constructs the OpenAPI document.
// schemaNames is passed at wire-up time and refreshed on each handler call
// in the live version (see router.go).
// openAPIOptions says which optional parts of the engine this build serves.
// Not every route the engine can mount is mounted in every build, and a
// document describing one the router never mounted is a lie a client acts on.
type openAPIOptions struct {
	// TenantFeatures is true when a licensing implementation supplied the
	// withheld-feature store, which is what mounts those two routes.
	TenantFeatures bool

	// LicensePresentation is true when the licensing implementation serves
	// its presentation at licensing.PresentationPath.
	LicensePresentation bool

	// AdminTokens is true when a plugin supplied somewhere to keep admin
	// tokens, which is what mounts the six routes that issue, rotate, revoke
	// and audit one.
	AdminTokens bool

	// OwnerRoutes are the routes the router mounts for owners that are not
	// plugins, by owner.
	OwnerRoutes []plugin.PluginRoutes

	// Ungated reports whether a plugin starts whatever the license says, so
	// its routes name no license feature. Nil reports false for every plugin.
	Ungated func(pluginName string) bool
}

func buildOpenAPIDoc(schemaNames []string, plugins []plugin.PluginRoutes, opts openAPIOptions) openAPIDoc {
	bearerAuth := map[string][]any{"bearerAuth": {}}
	cookieAuth := map[string][]any{"cookieAuth": {}}
	bothAuth := []map[string][]any{bearerAuth, cookieAuth}

	jsonBody := func(example map[string]any) *openAPIRequestBody {
		return &openAPIRequestBody{
			Required: true,
			Content: map[string]openAPIMediaType{
				"application/json": {
					Schema: map[string]any{"type": "object", "example": example},
				},
			},
		}
	}

	ok200 := map[string]any{"description": "OK"}
	err400 := map[string]any{"description": "Bad request"}
	err401 := map[string]any{"description": "Unauthorized"}
	err403 := map[string]any{"description": "Forbidden"}
	admins := []string{"admin", "super_admin"}
	super := []string{"super_admin"}

	paths := map[string]openAPIPath{}

	// Admin API: auth
	paths["/api/admin/setup"] = openAPIPath{
		"get": {
			Tags:      []string{"Admin / Auth"},
			Summary:   "Check setup status",
			Responses: map[string]any{"200": ok200},
		},
		"post": {
			Tags:        []string{"Admin / Auth"},
			Summary:     "Initial admin setup (first-run only)",
			Description: "Requires the setup token: LYEVE_SETUP_TOKEN, or the one-time token the engine logs at boot while no account exists. Send it as `setup_token` or in the `X-Setup-Token` header. Answers 409 once an account exists.",
			RequestBody: jsonBody(map[string]any{"email": "admin@example.com", "password": "changeme", "setup_token": "from-the-engine-log"}),
			Responses:   map[string]any{"201": ok200, "400": err400, "401": err401, "409": map[string]any{"description": "Setup already complete"}},
		},
	}
	paths["/api/admin/auth/sign-in-routes"] = openAPIPath{
		"get": {
			Tags:        []string{"Admin / Auth"},
			Summary:     "List the sign-in routes the plugins mounted",
			Description: "Public. Each entry is a method and a path under `/api/admin/auth/` that a plugin serves without a credential, such as a password reset request. A sign-in page offers a method only when its route is listed.",
			Responses:   map[string]any{"200": ok200},
		},
	}
	paths["/api/admin/auth/login"] = openAPIPath{
		"post": {
			Tags:        []string{"Admin / Auth"},
			Summary:     "Login and receive session cookie",
			Description: "Returns `mfa_required: true` and a `challenge_token` when the user has MFA enabled. Exchange the token via `/auth/mfa-verify`.",
			RequestBody: jsonBody(map[string]any{"email": "admin@example.com", "password": "changeme"}),
			Responses:   map[string]any{"200": ok200, "401": err401},
		},
	}
	paths["/api/admin/auth/logout"] = openAPIPath{
		"post": {
			Tags:      []string{"Admin / Auth"},
			Summary:   "Logout (clear session cookie)",
			Security:  bothAuth,
			Responses: map[string]any{"200": ok200},
		},
	}
	paths["/api/admin/auth/mfa-verify"] = openAPIPath{
		"post": {
			Tags:        []string{"Admin / Auth"},
			Summary:     "Complete MFA challenge - exchange challenge token + TOTP code for a full session JWT",
			RequestBody: jsonBody(map[string]any{"challenge_token": "<short-lived JWT>", "code": "123456"}),
			Responses:   map[string]any{"200": ok200, "401": err401, "422": map[string]any{"description": "Invalid code"}},
		},
	}
	paths["/api/admin/auth/me"] = openAPIPath{
		"get": {
			Tags:      []string{"Admin / Auth"},
			Summary:   "Current authenticated user",
			Security:  bothAuth,
			Responses: map[string]any{"200": ok200, "401": err401},
		},
	}
	paths["/api/admin/auth/memberships"] = openAPIPath{
		"get": {
			Tags:      []string{"Admin / Auth"},
			Summary:   "Tenants the caller may act in, home tenant first",
			Security:  bothAuth,
			Responses: map[string]any{"200": ok200, "401": err401},
		},
	}

	// The cross-tenant membership routes belong to whichever plugin serves
	// tenancy and are documented from its own route declarations, because a
	// build without one serves none of them.

	tenantParam := []openAPIParameter{{Name: "tenant", In: "path", Required: true, Schema: map[string]any{"type": "string"}}}

	// Admin API: tenant features. Only a build whose licensing implementation
	// keeps a withheld set mounts these two, so only such a build documents
	// them.
	if opts.TenantFeatures {
		paths["/api/admin/tenant-features/{tenant}"] = openAPIPath{
			"get": {
				Tags:       []string{"Admin / Tenant features"},
				Summary:    "Plugins withheld from one tenant, and every plugin that could be",
				Security:   bothAuth,
				Parameters: tenantParam,
				Responses:  map[string]any{"200": ok200, "400": err400, "401": err401},
			},
			"put": {
				Tags:        []string{"Admin / Tenant features"},
				Summary:     "Replace what is withheld from one tenant. The license stays the ceiling; an empty list gives the tenant all of it",
				Security:    bothAuth,
				Parameters:  tenantParam,
				RequestBody: jsonBody(map[string]any{"withheld": []any{"widgets"}}),
				Responses:   map[string]any{"200": ok200, "400": err400, "401": err401, "422": map[string]any{"description": "A name that is not a plugin this instance serves"}},
			},
		}
	}

	// Admin API: the license presentation. Only a build whose licensing
	// implementation serves it documents it, in the words pkg/licensing gives
	// the route, so it reads the same whichever implementation serves it.
	if opts.LicensePresentation {
		decl := licensing.PresentationRoute(nil)
		paths[decl.Pattern] = openAPIPath{strings.ToLower(decl.Method): declaredRouteOperation(decl, "", "")}
	}

	// The admin schema routes are not described here. They belong to whichever
	// plugin registered the schema engine, and a plugin's routes reach this
	// document through its own declaration rather than through a literal the
	// engine keeps, so the document never promises a route this build does not
	// serve.
	// Admin API: users
	paths["/api/admin/users"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{
			Tags: []string{"Admin / Users"}, Summary: "List all users", Security: bothAuth,
			Responses: map[string]any{"200": ok200, "401": err401, "403": err403},
		}, super...),
		"post": requiringRoles(&openAPIOperation{
			Tags: []string{"Admin / Users"}, Summary: "Create a user", Security: bothAuth,
			RequestBody: &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{
				"type":     "object",
				"required": []string{"email", "password", "roles"},
				"properties": map[string]any{
					"email":    map[string]any{"type": "string", "format": "email"},
					"password": map[string]any{"type": "string"},
					"roles": map[string]any{
						"type": "array", "minItems": 1, "items": map[string]any{"type": "string"},
						"description": "The roles the account holds. There is no default, so a request with no roles is refused.",
					},
				},
				"example": map[string]any{"email": "user@example.com", "password": "pass", "roles": []string{"editor"}},
			}}}},
			Responses: map[string]any{"201": map[string]any{"description": "Created"}, "401": err401, "403": err403,
				"422": map[string]any{"description": "A field is missing or invalid: no email, a password the policy refuses, or no roles"}},
		}, super...),
	}

	// Admin API: the provider window of the request's tenant
	paths["/api/admin/auth/provider-window"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{
			Tags: []string{"Admin / Auth"}, Summary: "Days an account created by the tenant's identity provider may still sign in by password, reset or magic link after the provider last signed it in", Security: bothAuth,
			Responses: map[string]any{"200": ok200, "401": err401, "403": err403},
		}, admins...),
		"put": requiringRoles(&openAPIOperation{
			Tags: []string{"Admin / Auth"}, Summary: "Set the provider window, a whole number of days from 0 to 365. 0 means only through the identity provider", Security: bothAuth,
			RequestBody: jsonBody(map[string]any{"days": 30}),
			Responses:   map[string]any{"200": ok200, "401": err401, "403": err403, "422": map[string]any{"description": "Days outside 0 to 365, or not a whole number"}},
		}, admins...),
	}

	// Admin API: configuration
	paths["/api/admin/config"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{
			Tags:      []string{"Admin / Configuration"},
			Summary:   "Report every setting and the layer it comes from",
			Security:  bothAuth,
			Responses: map[string]any{"200": ok200, "401": err401, "403": err403},
		}, super...),
		"put": requiringRoles(&openAPIOperation{
			Tags:     []string{"Admin / Configuration"},
			Summary:  "Store engine settings in the admin layer",
			Security: bothAuth,
			Responses: map[string]any{
				"200": ok200, "400": err400, "401": err401, "403": err403,
				"409": map[string]any{"description": "Every submitted key is pinned by a higher layer"},
				"412": map[string]any{"description": "A credential was submitted with no encryption key configured"},
			},
		}, super...),
	}

	paths["/api/admin/schemas/export"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{
			Tags:     []string{"Admin / Content Types"},
			Summary:  "Export every content type as a portable bundle",
			Security: bothAuth,
			Parameters: []openAPIParameter{
				{Name: "format", In: "query", Schema: map[string]any{"type": "string", "enum": []any{"yaml", "json"}, "default": "yaml"}},
			},
			Responses: map[string]any{
				"200": openAPIResponse{
					Description: "The bundle: a YAML file to download, or JSON when format is json",
					Content: map[string]openAPIMediaType{
						"application/yaml": {Schema: map[string]any{"type": "string"}},
						"application/json": {Schema: map[string]any{"type": "object"}},
					},
				},
				"401": err401, "403": err403, "503": map[string]any{"description": "Schema engine unavailable"},
			},
		}, admins...),
	}
	paths["/api/admin/schemas/import"] = openAPIPath{
		"post": requiringRoles(&openAPIOperation{
			Tags:    []string{"Admin / Content Types"},
			Summary: "Import content types from a bundle or another system",
			Description: "Reports what would change. Nothing is written unless apply=true, because " +
				"the statements a schema change generates include destructive ones.",
			Security: bothAuth,
			Parameters: []openAPIParameter{
				{Name: "from", In: "query", Schema: map[string]any{
					"type": "string", "default": "lyeve",
					"enum": []any{"lyeve", "json-schema", "openapi", "strapi", "contentful", "wordpress"},
				}},
				{Name: "apply", In: "query", Schema: map[string]any{"type": "boolean", "default": false}},
			},
			RequestBody: schemaImportBody(),
			Responses: map[string]any{
				"200": ok200, "400": err400, "401": err401, "403": err403,
				"409": map[string]any{"description": "References cannot be satisfied; nothing was applied"},
				"413": map[string]any{"description": "The definitions are larger than the request body limit"},
				"422": map[string]any{"description": "Definitions could not be read"},
				"503": map[string]any{"description": "Schema engine unavailable"},
			},
		}, super...),
	}

	// Admin API: GDPR DSAR
	notASubject422 := map[string]any{"description": "The identifier is not an email address, an account id or an IP address"}
	paths["/api/admin/gdpr/export"] = openAPIPath{
		"post": requiringRoles(&openAPIOperation{Tags: []string{"Admin / GDPR"}, Summary: "Export all data for a data subject (Art.15/20 portable bundle)", Security: bothAuth, RequestBody: jsonBody(map[string]any{"identifier": "user@example.com"}), Responses: map[string]any{"200": ok200, "400": err400, "401": err401, "403": err403, "422": notASubject422,
			"503": map[string]any{"description": "The subject's addresses could not be read, so nothing was exported. Repeat the request"},
		}}, super...),
	}
	// One body for both answers: complete says whether the subject is fully
	// erased, and a partial erasure answers 207 so no caller reads it as done.
	eraseBody := map[string]openAPIMediaType{"application/json": {Schema: map[string]any{
		"type":     "object",
		"required": []string{"identifier", "total_rows", "complete"},
		"properties": map[string]any{
			"identifier": map[string]any{"type": "string"},
			"total_rows": map[string]any{"type": "integer", "description": "Rows erased or anonymized"},
			"complete":   map[string]any{"type": "boolean", "description": "True only when every part erased and no legal hold kept any"},
			"failed":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The owners that failed or did not run"},
			"errors":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "What is unfinished, that the account was not erased, and that repeating the request finishes it"},
			"holds":      map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Legal holds that kept data"},
		},
	}}}
	paths["/api/admin/gdpr/erase"] = openAPIPath{
		"post": requiringRoles(&openAPIOperation{Tags: []string{"Admin / GDPR"}, Summary: "Erase or anonymize a data subject's data (Art.17 right to erasure)", Security: bothAuth, RequestBody: jsonBody(map[string]any{"identifier": "user@example.com"}), Responses: map[string]any{
			"200": openAPIResponse{Description: "Erased. complete is false when a legal hold kept data", Content: eraseBody},
			"207": openAPIResponse{Description: "Partly erased: either the account was not erased, failed names the owners left, and repeating the request finishes it, or some rows were kept because they cannot be told apart from another account's data", Content: eraseBody},
			"400": err400, "401": err401, "403": err403, "422": notASubject422,
			"503": map[string]any{"description": "Legal hold status is unavailable or the subject could not be signed out, so nothing was erased. Repeat the request"},
		}}, super...),
	}

	// Admin API: probe endpoints
	// These probes take any credential the engine accepts, a key minted for
	// content work included. An orchestrator's probes are /healthz, /readyz
	// and /startup, which take none.
	paths["/api/admin/health"] = openAPIPath{
		"get": {Tags: []string{"System"}, Summary: "Liveness probe - is the process alive?", Security: bothAuth, Responses: map[string]any{"200": ok200, "401": err401, "503": map[string]any{"description": "Database unreachable"}}},
	}
	paths["/api/admin/ready"] = openAPIPath{
		"get": {Tags: []string{"System"}, Summary: "Readiness probe - is the server ready to serve traffic?", Security: bothAuth, Responses: map[string]any{"200": ok200, "401": err401, "503": map[string]any{"description": "Not ready"}}},
	}
	paths["/api/admin/metrics"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{
			Tags:        []string{"System"},
			Summary:     "Prometheus metrics",
			Description: "A scraper presents the metrics token and needs no account. A super admin's session reaches the route too. While METRICS_TOKEN is set, any other bearer token is refused, so that session has to come from the cookie.",
			Security:    []map[string][]any{{"metricsToken": {}}, bearerAuth, cookieAuth},
			Responses: map[string]any{
				"200": openAPIResponse{
					Description: "The Prometheus text format, version 0.0.4, or the delimited protobuf format when the Accept header asks for it",
					Content: map[string]openAPIMediaType{
						"text/plain":                {Schema: map[string]any{"type": "string"}},
						prometheusProtobufMediaType: {},
					},
				},
				"401": map[string]any{"description": "No credential, or a bearer token other than the metrics token while one is set"},
				"403": map[string]any{"description": "A session without the super_admin role"},
			},
		}, super...),
	}
	paths["/api/admin/pool/health"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{Tags: []string{"Admin / System"}, Summary: "Connection pool health - live statistics (open/in-use/idle connections, latency, prepared statement cache)", Security: bothAuth, Responses: map[string]any{"200": ok200, "401": err401, "403": err403, "503": map[string]any{"description": "Pool unhealthy"}}}, admins...),
	}
	paths["/api/admin/openapi.json"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{Tags: []string{"Admin / System"}, Summary: "OpenAPI 3.1 specification: the whole document for a super admin, the public half for an admin", Security: bothAuth, Responses: map[string]any{"200": map[string]any{"description": "OpenAPI 3.1 JSON"}, "401": err401, "403": err403}}, admins...),
	}
	paths["/api/admin/openapi/public.json"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{Tags: []string{"Admin / System"}, Summary: "OpenAPI 3.1 specification of the content API and every route outside /api/admin", Security: bothAuth, Responses: map[string]any{"200": map[string]any{"description": "OpenAPI 3.1 JSON"}, "401": err401, "403": err403}}, admins...),
	}
	paths["/api/admin/openapi/admin.json"] = openAPIPath{
		"get": requiringRoles(&openAPIOperation{Tags: []string{"Admin / System"}, Summary: "OpenAPI 3.1 specification of the /api/admin routes", Security: bothAuth, Responses: map[string]any{"200": map[string]any{"description": "OpenAPI 3.1 JSON"}, "401": err401, "403": map[string]any{"description": "Not a super admin"}}}, super...),
	}

	// The content API, once, as the {schema} family the router registers.
	documentContentFamily(paths, schemaNames)

	// Content API: auth
	paths["/api/v1/auth/token"] = openAPIPath{
		"post": {
			Tags:        []string{"Content API / Auth"},
			Summary:     "Obtain a Bearer token",
			Description: "Returns `mfa_required: true` and a `challenge_token` in place of a token when the user has MFA enabled. Exchange the challenge for a session via `/api/admin/auth/mfa-verify`.",
			RequestBody: jsonBody(map[string]any{"email": "user@example.com", "password": "changeme"}),
			Responses:   map[string]any{"200": ok200, "401": err401},
		},
	}
	paths["/api/v1/health"] = openAPIPath{
		"get": {Tags: []string{"System"}, Summary: "API server liveness probe", Security: []map[string][]any{bearerAuth}, Responses: map[string]any{"200": ok200, "401": err401}},
	}
	paths["/api/v1/ready"] = openAPIPath{
		"get": {Tags: []string{"System"}, Summary: "API server readiness probe", Security: []map[string][]any{bearerAuth}, Responses: map[string]any{"200": ok200, "401": err401}},
	}
	// The remaining engine admin routes, then the routes the active plugins
	// and the other owners declare. None overwrites an entry written before
	// it.
	documentAdminGaps(paths)
	// Admin tokens are served only when a plugin supplies somewhere to keep
	// them, so a document that named them either way would advertise routes
	// this build answers 404 to.
	if opts.AdminTokens {
		documentAdminTokens(paths)
	}
	documentDeviceLogin(paths)
	documentPluginRoutes(paths, plugins, opts.OwnerRoutes, opts.Ungated)
	annotateAdminGrants(paths, append(plugins[:len(plugins):len(plugins)], opts.OwnerRoutes...))
	return openAPIDoc{
		OpenAPI: "3.1.0",
		Info: openAPIInfo{
			Title:       "LyEve API",
			Description: "Headless CMS - Admin API (cookie + bearer) and Content API (bearer only).",
			Version:     "1.0.0",
		},
		Servers: []openAPIServer{
			{URL: "http://localhost:3001", Description: "Admin server (local dev)"},
			{URL: "http://localhost:3002", Description: "Content API server (local dev)"},
		},
		Paths: paths,
		Components: openAPIComponents{
			SecuritySchemes: map[string]any{
				"bearerAuth": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "JWT",
				},
				"cookieAuth": map[string]any{
					"type":        "apiKey",
					"in":          "cookie",
					"name":        security.SessionCookieName,
					"description": "Session cookie set at sign-in. A server running without secure cookies names it " + security.SessionCookieNameInsecure + ".",
				},
				"adminToken":   adminTokenScheme,
				"metricsToken": metricsTokenScheme,
			},
		},
	}
}

// schemaImportBody documents what the import reads: a bundle as the export
// writes it, or the definitions another system exports, in YAML or JSON,
// under each media type the router admits on the import's path. The JSON is
// left untyped because a source may export a list as well as an object.
func schemaImportBody() *openAPIRequestBody {
	content := map[string]openAPIMediaType{
		"application/json": {Schema: map[string]any{"example": map[string]any{
			"version": schemaimport.BundleVersion,
			"schemas": []any{map[string]any{
				"name": "notes", "display_name": "Notes",
				"fields": []any{map[string]any{"name": "title", "field_type": "text"}},
			}},
		}}},
	}
	for _, mediaType := range schemaImportMediaTypes {
		content[mediaType] = openAPIMediaType{Schema: map[string]any{"type": "string"}}
	}
	return &openAPIRequestBody{
		Description: "A bundle as GET /api/admin/schemas/export writes it, or the definitions of the system the from parameter names, in YAML or JSON.",
		Required:    true,
		Content:     content,
	}
}
