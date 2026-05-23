package api

import (
	"sort"
	"strconv"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// The document is the one place a caller learns what the engine serves, and
// the router test holds it to every registered route. What follows is the
// content family, documented once as the {schema} pattern the router
// registers rather than once per collection, the remaining engine admin
// routes, and the routes the active plugins declare.

// A collection list past this length is stated as a count. The names are
// what the schema endpoints are for, and a long enum on a path parameter
// makes the document unreadable and the admin's reference page unusable.
const openAPICollectionEnumMax = 50

// xRoles is the role a route requires, carried on the operation as an
// extension so a client can show the gate without knowing the router.
const xRoles = "x-roles"

// xFeature is the license feature a route needs, carried on the operation
// so a client can tell a route the license does not cover from one that does
// not exist. A route of a plugin its grant marks ungated needs no feature and
// carries none, and so does an engine route, because no declaration names
// one.
const xFeature = "x-feature"

// requiringFeature records on the operation the feature the route needs.
func requiringFeature(op *openAPIOperation, feature string) *openAPIOperation {
	if op.Extensions == nil {
		op.Extensions = map[string]any{}
	}
	op.Extensions[xFeature] = feature
	return op
}

// requiringRoles records on the operation which role the router demands.
func requiringRoles(op *openAPIOperation, roles ...string) *openAPIOperation {
	if op.Extensions == nil {
		op.Extensions = map[string]any{}
	}
	op.Extensions[xRoles] = roles
	return op
}

func schemaPathParam(collections []string) openAPIParameter {
	p := openAPIParameter{
		Name:        "schema",
		In:          "path",
		Required:    true,
		Description: "Collection name, as listed by GET /api/v1/schemas.",
		Schema:      map[string]any{"type": "string"},
	}
	switch n := len(collections); {
	case n == 0:
	case n <= openAPICollectionEnumMax:
		names := append([]string(nil), collections...)
		sort.Strings(names)
		vals := make([]any, len(names))
		for i, s := range names {
			vals[i] = s
		}
		p.Schema["enum"] = vals
	default:
		p.Description = "Collection name, as listed by GET /api/v1/schemas. This instance has " + strconv.Itoa(n) + " collections."
	}
	return p
}

// documentContentFamily writes the content API once, as the routes the API
// server registers. Every operation takes the collection as a path parameter.
// The write operations carry the roles the router requires.
func documentContentFamily(paths map[string]openAPIPath, collections []string) {
	const tag = "Content API / Content"
	bearer := []map[string][]any{{"bearerAuth": {}}}
	ok200 := map[string]any{"description": "OK"}
	err401 := map[string]any{"description": "Unauthorized"}
	err404 := map[string]any{"description": "Not found"}
	writers := []string{"editor", "admin", "super_admin"}
	schema := schemaPathParam(collections)
	id := openAPIParameter{Name: "id", In: "path", Required: true, Description: "Record id", Schema: map[string]any{"type": "string", "format": "uuid"}}
	field := openAPIParameter{Name: "field", In: "path", Required: true, Description: "A relation field of the collection", Schema: map[string]any{"type": "string"}}
	body := &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "object"}}}}
	populate := openAPIParameter{Name: "populate", In: "query", Description: "Relation fields to resolve inline, comma-separated, or * for all.", Schema: map[string]any{"type": "string"}}
	depth := openAPIParameter{Name: "depth", In: "query", Description: "How many levels of relations to resolve.", Schema: map[string]any{"type": "integer", "minimum": 0}}
	locale := openAPIParameter{Name: "locale", In: "query", Description: "The locale to serve the record's fields in, a language tag such as fr or pt-BR. Without it the Accept-Language header's preferred language is used. Honored only when a localizer is registered: each record's fields are then merged with its translation and the record carries resolved_locale, the locale the fields came from after the tenant's fallback chain. Otherwise the parameter is ignored and the response is unchanged.", Schema: map[string]any{"type": "string"}}
	err503Locale := map[string]any{"description": "The translations could not be read"}

	const base = "/api/v1/content/{schema}"
	paths[base] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "List records", Security: bearer,
			Parameters: []openAPIParameter{schema,
				{Name: "limit", In: "query", Description: "Maximum records per page. Clamped to 1-200 (default 25). Values above 200 are silently reduced to 200 and bypass the list cache. For traversing large result sets, use the cursor endpoint instead.", Schema: map[string]any{"type": "integer", "default": contentListDefaultLimit, "minimum": contentListMinLimit, "maximum": contentListMaxLimit}},
				{Name: "offset", In: "query", Schema: map[string]any{"type": "integer", "default": 0}},
				populate, depth, locale},
			Responses: map[string]any{"200": ok200, "401": err401, "404": err404, "503": err503Locale},
		},
		"post": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Create a record", Security: bearer,
			Parameters:  []openAPIParameter{schema},
			RequestBody: body,
			Responses:   map[string]any{"201": map[string]any{"description": "Created"}, "401": err401, "422": map[string]any{"description": "Hook validation failed"}},
		}, writers...),
	}
	paths[base+"/cursor"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "List records by cursor", Description: "Keyset pagination for large result sets: pass the cursor the previous page returned.", Security: bearer,
			Parameters: []openAPIParameter{schema,
				{Name: "cursor", In: "query", Description: "The cursor from the previous page; empty for the first page.", Schema: map[string]any{"type": "string"}},
				{Name: "limit", In: "query", Schema: map[string]any{"type": "integer", "default": contentListDefaultLimit, "minimum": contentListMinLimit, "maximum": contentListMaxLimit}},
				locale},
			Responses: map[string]any{"200": ok200, "401": err401, "404": err404, "503": err503Locale},
		},
	}
	paths[base+"/stream"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "Stream records", Description: "Newline-delimited JSON, one record per line, for exports too large to page.", Security: bearer,
			Parameters: []openAPIParameter{schema},
			Responses:  map[string]any{"200": map[string]any{"description": "application/x-ndjson"}, "401": err401, "404": err404},
		},
	}
	paths[base+"/bulk"] = openAPIPath{
		"post": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Create records in bulk", Security: bearer,
			Parameters:  []openAPIParameter{schema},
			RequestBody: &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}}},
			Responses:   map[string]any{"201": map[string]any{"description": "Created"}, "401": err401, "422": map[string]any{"description": "Validation failed"}},
		}, writers...),
	}
	paths[base+"/{id}"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "Get a record", Security: bearer,
			Parameters: []openAPIParameter{schema, id, populate, depth, locale},
			Responses:  map[string]any{"200": ok200, "401": err401, "404": err404, "503": err503Locale},
		},
		"put": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Update a record", Security: bearer,
			Parameters: []openAPIParameter{schema, id}, RequestBody: body,
			Responses: map[string]any{"200": ok200, "401": err401, "404": err404},
		}, writers...),
		"delete": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Delete a record", Security: bearer,
			Parameters: []openAPIParameter{schema, id},
			Responses:  map[string]any{"204": map[string]any{"description": "Deleted"}, "401": err401, "404": err404},
		}, writers...),
	}
	paths[base+"/{id}/relations/{field}"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "List the records a relation field points at", Security: bearer,
			Parameters: []openAPIParameter{schema, id, field},
			Responses:  map[string]any{"200": ok200, "401": err401, "404": err404},
		},
		"put": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Replace the records a relation field points at", Security: bearer,
			Parameters:  []openAPIParameter{schema, id, field},
			RequestBody: &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "object", "example": map[string]any{"ids": []string{"<uuid>", "<uuid>"}}}}}},
			Responses:   map[string]any{"200": ok200, "401": err401, "404": err404},
		}, writers...),
	}
	paths[base+"/{id}/revisions"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "List a record's revisions", Security: bearer,
			Parameters: []openAPIParameter{schema, id},
			Responses:  map[string]any{"200": ok200, "401": err401, "404": err404},
		},
	}
	paths[base+"/{id}/revisions/{rev_id}/restore"] = openAPIPath{
		"put": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Restore a revision", Description: "The record's current state becomes a new revision first, so a restore is itself undoable.", Security: bearer,
			Parameters: []openAPIParameter{schema, id, {Name: "rev_id", In: "path", Required: true, Description: "Revision id", Schema: map[string]any{"type": "string", "format": "uuid"}}},
			Responses:  map[string]any{"200": ok200, "401": err401, "404": err404},
		}, writers...),
	}
	paths[base+"/{id}/publish"] = openAPIPath{
		"put": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Publish a record", Security: bearer,
			Parameters: []openAPIParameter{schema, id},
			Responses:  map[string]any{"200": ok200, "401": err401, "404": err404},
		}, writers...),
	}
	paths[base+"/{id}/unpublish"] = openAPIPath{
		"put": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Unpublish a record", Security: bearer,
			Parameters: []openAPIParameter{schema, id},
			Responses:  map[string]any{"200": ok200, "401": err401, "404": err404},
		}, writers...),
	}
}

// documentAdminGaps documents the engine's remaining admin routes, each with
// the roles the router requires.
func documentAdminGaps(paths map[string]openAPIPath) {
	both := []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
	bearer := []map[string][]any{{"bearerAuth": {}}}
	ok200 := map[string]any{"description": "OK"}
	err401 := map[string]any{"description": "Unauthorized"}
	err403 := map[string]any{"description": "Forbidden"}
	err404 := map[string]any{"description": "Not found"}
	admins := []string{"admin", "super_admin"}
	super := []string{"super_admin"}
	name := openAPIParameter{Name: "name", In: "path", Required: true, Schema: map[string]any{"type": "string"}}
	id := openAPIParameter{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string", "format": "uuid"}}
	jsonObj := &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "object"}}}}
	set := func(path, method string, op *openAPIOperation) {
		if paths[path] == nil {
			paths[path] = openAPIPath{}
		}
		if paths[path][method] == nil {
			paths[path][method] = op
		}
	}

	set("/api/admin/auth/refresh", "post", &openAPIOperation{
		Tags: []string{"Admin / Auth"}, Summary: "Refresh the session", Description: "Rotates the refresh token and issues a new access token. Rate limited.",
		Responses: map[string]any{"200": ok200, "401": err401},
	})
	// The schema and migrate operations are declared by the plugin that serves
	// them, not here.
	set("/api/admin/users/{id}/roles", "put", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Users"}, Summary: "Set a user's roles", Security: both,
		Parameters: []openAPIParameter{id}, RequestBody: &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "object", "example": map[string]any{"roles": []string{"editor"}}}}}},
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403, "404": err404},
	}, super...))
	set("/api/admin/users/{id}/state", "put", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Users"}, Summary: "Lock or unlock a user", Security: both,
		Parameters: []openAPIParameter{id}, RequestBody: &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "object", "example": map[string]any{"locked": true}}}}},
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403, "404": err404},
	}, super...))
	set("/api/admin/users/{id}/password", "put", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Users"}, Summary: "Set a user's password; every session the account held ends", Security: both,
		Parameters: []openAPIParameter{id}, RequestBody: &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "object", "example": map[string]any{"password": "a-new-password-of-twelve"}}}}},
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403, "404": err404, "422": map[string]any{"description": "Password policy not met"}},
	}, super...))
	set("/api/admin/users/{id}", "delete", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Users"}, Summary: "Delete a user", Security: both,
		Parameters: []openAPIParameter{id},
		Responses:  map[string]any{"204": map[string]any{"description": "Deleted"}, "401": err401, "403": err403, "404": err404},
	}, super...))
	set("/api/admin/plugins/status", "get", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Plugins"}, Summary: "Every plugin's phase, start time and last error", Description: pluginStatusDescription(), Security: both,
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403},
	}, admins...))
	set("/api/admin/plugins/running", "get", &openAPIOperation{
		Tags: []string{"Admin / Plugins"}, Summary: "The plugins that serve the caller's tenant", Description: "Any signed-in role. plugins is every compiled plugin that is running or starts on first use, less the ones withheld from the caller's tenant. withheld is every plugin that runs but is withheld from it. Names only, sorted, and neither list is ever null.", Security: both,
		Responses: map[string]any{"200": ok200, "401": err401},
	})
	set("/api/admin/plugins/{name}/schema", "get", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Plugins"}, Summary: "JSON Schema of a plugin's configuration", Security: both,
		Parameters: []openAPIParameter{name},
		Responses:  map[string]any{"200": ok200, "401": err401, "404": err404},
	}, admins...))
	set("/api/admin/plugins/{name}/config", "get", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Plugins"}, Summary: "A plugin's configuration, secrets masked", Security: both,
		Parameters: []openAPIParameter{name},
		Responses:  map[string]any{"200": ok200, "401": err401, "404": err404},
	}, admins...))
	set("/api/admin/plugins/{name}/config", "put", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Plugins"}, Summary: "Replace a plugin's configuration", Description: "Validated against the plugin's schema; secret values are sealed at rest.", Security: both,
		Parameters: []openAPIParameter{name}, RequestBody: jsonObj,
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403, "422": map[string]any{"description": "Validation failed"}},
	}, super...))
	set("/api/admin/plugins/{name}/config/reset", "post", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Plugins"}, Summary: "Reset a plugin's configuration to its defaults", Security: both,
		Parameters: []openAPIParameter{name},
		Responses:  map[string]any{"200": ok200, "401": err401, "403": err403},
	}, super...))
	set("/api/admin/entitlements", "get", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / System"}, Summary: "What the license entitles", Description: "Plan, state, feature list and caps, with license_source, which says where the license came from (token, key or stored_key), expires_at, and license_error, which says why the configured license is not in force. A licensing implementation may add plan_label, the plan as an operator reads it, and grace_ends_at, which it sets only in grace. license_module is always present, and true when the build links a licensing implementation. Never the token or the key itself.", Security: both,
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403},
	}, admins...))
	set("/api/admin/api-key-scopes", "get", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / System"}, Summary: "Every route an API key can be scoped to", Description: "One row per route: method, path, the scope's resource, the name that qualifies it (a path parameter such as {schema}, a literal endpoint, or none), the action, the narrowest scope that reaches the route, and the owner. A route marked declared names its own scope, which takes no qualifier. A scope on the bare resource covers every name under it.", Security: both,
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403},
	}, admins...))
	set("/api/admin/security/controls", "get", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / System"}, Summary: "Which security controls are enforcing", Description: "One row per control: pass, warn, fail or skip, the detail behind it, and what turns a control on that is not enforcing. Read live, so a plugin the license activates after boot shows without a restart.", Security: both,
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403},
	}, super...))
	goroutineTag := []string{"Admin / System"}
	set("/api/admin/debug/goroutines", "get", requiringRoles(&openAPIOperation{
		Tags: goroutineTag, Summary: "Tracked goroutines", Description: "The tracker snapshot: tracked and runtime totals, counts by source name, and by_owner, one row per plugin or the engine with its live count and the age of its oldest goroutine, so a leak names who started it.", Security: both,
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403},
	}, admins...))
	set("/robots.txt", "get", &openAPIOperation{
		Tags: []string{"Admin / System"}, Summary: "Refuses every crawler", Description: "Every route on this server is authenticated, so there is nothing to index; the same refusal rides on every response as X-Robots-Tag.",
		Responses: map[string]any{"200": map[string]any{"description": "A robots.txt disallowing every path"}},
	})
	set("/.well-known/jwks.json", "get", &openAPIOperation{
		Tags: []string{"Content API / Auth"}, Summary: "The signing keys, as a JWK Set", Description: "Verify a token the engine issued without calling it: every key the engine signs with, current and rotated, by kid.",
		Responses: map[string]any{"200": ok200},
	})
	set("/api/v1/schemas", "get", &openAPIOperation{
		Tags: []string{"Content API / Schemas"}, Summary: "List collections", Security: bearer,
		Responses: map[string]any{"200": ok200, "401": err401},
	})
	set("/api/v1/schemas/{name}", "get", &openAPIOperation{
		Tags: []string{"Content API / Schemas"}, Summary: "Get a collection's definition", Security: bearer,
		Parameters: []openAPIParameter{name},
		Responses:  map[string]any{"200": ok200, "401": err401, "404": err404},
	})
}

// pluginStatusDescription says what a plugin status row carries besides its
// phase, from the limits and the categories the engine applies, so the text
// cannot drift from them.
func pluginStatusDescription() string {
	cats := core.PluginCategories()
	names := make([]string, len(cats))
	for i, c := range cats {
		names[i] = string(c)
	}
	return "One row per compiled plugin. A plugin that describes itself carries a manifest: a label of at most " +
		strconv.Itoa(core.ManifestLabelMax) + " characters, a description of at most " + strconv.Itoa(core.ManifestDescriptionMax) +
		", a category (" + strings.Join(names, ", ") + ") and a maturity (" + string(core.MaturityStable) + " or " + string(core.MaturityBeta) +
		"). A plugin that does not is listed by its name."
}

// documentPluginRoutes writes the routes the active plugins declared, and
// the routes of owners that are not plugins. A declaration carries the
// method, the pattern and the route group, so the document carries those,
// and the route's Doc adds what its author wrote. A route the engine
// documents itself keeps the engine's text, because the engine's handler is
// the one that serves it. A plugin its grant does not mark ungated is a
// license feature, so each of its routes carries the key. An owner that is
// not a plugin is no feature, and its name is the tag its routes default to.
//
// Two passes fill the paths, and neither replaces an entry already written:
// first the routes that carry a Doc, then every other route from its method
// and pattern. A route two owners declare is therefore described by the
// declaration that carries a Doc.
func documentPluginRoutes(paths map[string]openAPIPath, plugins, owners []plugin.PluginRoutes, ungated func(string) bool) {
	documentDeclaredRoutes(paths, plugins, owners, ungated, true)
	documentDeclaredRoutes(paths, plugins, owners, ungated, false)
}

// documentDeclaredRoutes writes the declared routes that carry a Doc, or the
// ones that carry none.
func documentDeclaredRoutes(paths map[string]openAPIPath, plugins, owners []plugin.PluginRoutes, ungated func(string) bool, withDoc bool) {
	for _, p := range plugins {
		feature := ""
		if ungated == nil || !ungated(p.Name) {
			feature = p.Name
		}
		documentRouteSet(paths, p.Routes, "Plugin / "+p.Name, feature, withDoc)
	}
	for _, o := range owners {
		documentRouteSet(paths, o.Routes, o.Name, "", withDoc)
	}
}

// documentRouteSet writes one owner's routes, those that carry a Doc or those
// that carry none, under tag and feature.
func documentRouteSet(paths map[string]openAPIPath, routes []core.RouteDecl, tag, feature string, withDoc bool) {
	for _, r := range routes {
		method := strings.ToLower(r.Method)
		if method == "" || r.Pattern == "" || (r.Doc != nil) != withDoc {
			continue
		}
		if paths[r.Pattern] == nil {
			paths[r.Pattern] = openAPIPath{}
		}
		if paths[r.Pattern][method] != nil {
			continue
		}
		paths[r.Pattern][method] = declaredRouteOperation(r, tag, feature)
	}
}

// declaredRouteOperation documents one declared route. The security, the
// roles and the license feature follow from its group and owner, whatever its
// Doc says.
func declaredRouteOperation(r core.RouteDecl, tag, feature string) *openAPIOperation {
	op := &openAPIOperation{
		Tags:       []string{tag},
		Summary:    strings.ToUpper(r.Method) + " " + r.Pattern,
		Parameters: pathParams(r.Pattern),
		Responses:  map[string]any{"200": map[string]any{"description": "OK"}},
	}
	if r.Doc != nil {
		applyRouteDoc(op, r.Pattern, r.Doc)
	}
	switch r.Group {
	case core.GroupPublic:
	case core.GroupAuth:
		op.Security = []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
	case core.GroupAdmin:
		op.Security = []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
		requiringRoles(op, "admin", "super_admin")
	case core.GroupSuperAdmin:
		op.Security = []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
		requiringRoles(op, "super_admin")
	}
	if feature != "" {
		requiringFeature(op, feature)
	}
	return op
}

// applyRouteDoc writes what a route's Doc says over the operation built from
// its method and pattern. A field the Doc leaves empty keeps what was built.
func applyRouteDoc(op *openAPIOperation, pattern string, d *core.RouteDoc) {
	if d.Tag != "" {
		op.Tags = []string{d.Tag}
	}
	if d.Summary != "" {
		op.Summary = d.Summary
	}
	op.Description = d.Description
	if len(d.Params) > 0 {
		op.Parameters = docParameters(pattern, d.Params)
	}
	if d.RequestExample != nil {
		op.RequestBody = &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{
			"application/json": {Schema: map[string]any{"type": "object", "example": d.RequestExample}},
		}}
	}
	if len(d.Responses) > 0 {
		op.Responses = make(map[string]any, len(d.Responses))
		for status, description := range d.Responses {
			op.Responses[strconv.Itoa(status)] = map[string]any{"description": description}
		}
	}
}

// docParameters lists the parameters a Doc names, after every path segment of
// the pattern it leaves out. OpenAPI requires each segment to be documented,
// so an omission cannot drop one, and a path parameter is required whatever
// the Doc says.
func docParameters(pattern string, params []core.RouteParam) []openAPIParameter {
	named := map[string]bool{}
	for _, p := range params {
		if p.In == "path" {
			named[p.Name] = true
		}
	}
	var out []openAPIParameter
	for _, p := range pathParams(pattern) {
		// A segment such as {id:[0-9]+} is named by what precedes the colon.
		name, _, _ := strings.Cut(p.Name, ":")
		if !named[name] {
			out = append(out, p)
		}
	}
	for _, p := range params {
		typ := p.Type
		if typ == "" {
			typ = "string"
		}
		schema := map[string]any{"type": typ}
		if p.Format != "" {
			schema["format"] = p.Format
		}
		if p.Default != nil {
			schema["default"] = p.Default
		}
		out = append(out, openAPIParameter{Name: p.Name, In: p.In, Required: p.Required || p.In == "path", Schema: schema})
	}
	return out
}

// documentEndpoints writes the endpoints plugins list for the caller's
// tenant, such as the URLs its flows answer. They are added after the
// declared routes and never replace one: an endpoint behind a declared
// pattern, such as a flow's slug URL, is a second operation of that path only
// when the method differs. ANY is written as the methods it accepts.
func documentEndpoints(paths map[string]openAPIPath, eps []core.DocumentedEndpoint) {
	for _, e := range eps {
		if e.Path == "" || e.Method == "" {
			continue
		}
		methods := []string{strings.ToLower(e.Method)}
		if strings.EqualFold(e.Method, "ANY") {
			methods = []string{"get", "post", "put", "patch", "delete"}
		}
		if paths[e.Path] == nil {
			paths[e.Path] = openAPIPath{}
		}
		for _, m := range methods {
			if paths[e.Path][m] != nil {
				continue
			}
			op := &openAPIOperation{
				Tags:        []string{e.Tag},
				Summary:     e.Summary,
				Description: e.Description,
				Parameters:  pathParams(e.Path),
				Responses:   map[string]any{"200": map[string]any{"description": "OK"}},
			}
			if e.Example != nil && m != "get" && m != "delete" {
				op.RequestBody = &openAPIRequestBody{Content: map[string]openAPIMediaType{
					"application/json": {Schema: map[string]any{"type": "object", "example": e.Example}},
				}}
			}
			switch e.Group {
			case core.GroupPublic:
			case core.GroupSuperAdmin:
				op.Security = []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
				requiringRoles(op, "super_admin")
			case core.GroupAdmin:
				op.Security = []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
				requiringRoles(op, "admin", "super_admin")
			default:
				op.Security = []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
			}
			if e.Feature != "" {
				requiringFeature(op, e.Feature)
			}
			paths[e.Path][m] = op
		}
	}
}

// pathParams reads the {name} segments of a pattern into path parameters.
func pathParams(pattern string) []openAPIParameter {
	var out []openAPIParameter
	for _, seg := range strings.Split(pattern, "/") {
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			out = append(out, openAPIParameter{Name: seg[1 : len(seg)-1], In: "path", Required: true, Schema: map[string]any{"type": "string"}})
		}
	}
	return out
}
