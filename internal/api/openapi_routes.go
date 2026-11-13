package api

import (
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/hooks"
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

// xEffect is what calling the route does to the engine's data, one of the
// values core.Effect names. An operation without it states nothing, and a
// caller has to assume it changes data.
const xEffect = "x-effect"

// describing names the operation and records its effect. An effect that is
// not one of the five is left out rather than published.
func describing(op *openAPIOperation, id string, effect core.Effect) *openAPIOperation {
	op.OperationID = id
	if effect.Valid() {
		if op.Extensions == nil {
			op.Extensions = map[string]any{}
		}
		op.Extensions[xEffect] = effect
	}
	return op
}

func schemaPathParam(collections []string) openAPIParameter {
	p := openAPIParameter{
		Name:        "schema",
		In:          "path",
		Required:    true,
		Description: "Content type name, as listed by GET /api/v1/schemas.",
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
		p.Description = "Content type name, as listed by GET /api/v1/schemas. This engine has " + strconv.Itoa(n) + " content types."
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
	field := openAPIParameter{Name: "field", In: "path", Required: true, Description: "A relation field of the content type", Schema: map[string]any{"type": "string"}}
	body := &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "object"}}}}
	populate := openAPIParameter{Name: "populate", In: "query", Description: "Relation fields to resolve inline, comma-separated, or * for all.", Schema: map[string]any{"type": "string"}}
	depth := openAPIParameter{Name: "depth", In: "query", Description: "How many levels of relations to resolve.", Schema: map[string]any{"type": "integer", "minimum": 0}}
	locale := openAPIParameter{Name: "locale", In: "query", Description: "The locale to serve the record's fields in, a language tag such as fr or pt-BR. Without it the Accept-Language header's preferred language is used. Honored only when a localizer is registered: each record's fields are then merged with its translation and the record carries resolved_locale, the locale the fields came from after the tenant's fallback chain. Otherwise the parameter is ignored and the response is unchanged.", Schema: map[string]any{"type": "string"}}
	fields := openAPIParameter{Name: "fields", In: "query", Description: "The fields of each record's data to return, comma-separated. A relation field keeps both its stored column and its populated record. Every record carries id, created_at and updated_at whatever the list names. A name this caller cannot read, because the content type has no such field or a field mask hides it, answers 400.", Schema: map[string]any{"type": "string"}}
	filters := openAPIParameter{Name: "filters", In: "query", Style: "deepObject", Explode: true, Description: "Records whose field equals a value, written filters[field]=value, or lies within a bound, written filters[field][gte]=value and filters[field][lte]=value, inclusive. A bound applies to created_at, updated_at and number, integer, decimal, date and datetime fields. A field the content type does not have, or a bound on any other field, answers 400, and one a field mask hides from this caller answers 403. The list serves published records unless filters[_status] names another status, which needs permission to read drafts.", Schema: map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}}
	sort := openAPIParameter{Name: "sort", In: "query", Description: "The field to order by, written sort=field, or sort=-field for the reverse order: created_at, updated_at, or a number, integer, decimal, date or datetime field. Records with no value come last either way, and records with equal values follow their id. Any other field answers 400, and one a field mask hides from this caller answers 403. Without it the list is newest first and the cursor list follows the record id.", Schema: map[string]any{"type": "string"}}
	err400 := map[string]any{"description": "A query parameter that cannot be read"}
	err503Locale := map[string]any{"description": "The translations could not be read"}
	err422 := map[string]any{"description": "Validation failed"}
	err503Rules := map[string]any{"description": "The content rules could not be checked, so nothing was stored"}

	const base = "/api/v1/content/{schema}"
	paths[base] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "List records", Security: bearer,
			Parameters: []openAPIParameter{schema,
				{Name: "limit", In: "query", Description: "Maximum records per page. Clamped to 1-200 (default 25). Values above 200 are silently reduced to 200 and bypass the list cache. For traversing large result sets, use the cursor endpoint instead.", Schema: map[string]any{"type": "integer", "default": contentListDefaultLimit, "minimum": contentListMinLimit, "maximum": contentListMaxLimit}},
				{Name: "offset", In: "query", Schema: map[string]any{"type": "integer", "default": 0}},
				filters, sort, populate, depth, locale, fields},
			Responses: map[string]any{"200": ok200, "400": err400, "401": err401, "403": map[string]any{"description": "A filter or the sort names a field this caller may not read"}, "404": err404, "503": err503Locale},
		},
		"post": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Create a record", Security: bearer,
			Parameters:  []openAPIParameter{schema},
			RequestBody: body,
			Responses:   map[string]any{"201": map[string]any{"description": "Created"}, "401": err401, "422": err422, "503": err503Rules},
		}, writers...),
	}
	paths[base+"/cursor"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "List records by cursor", Description: "Keyset pagination for large result sets: pass the cursor the previous page returned, with the same sort on every page. A sorted page whose cursor names a record that no longer exists answers 400.", Security: bearer,
			Parameters: []openAPIParameter{schema,
				{Name: "cursor", In: "query", Description: "The cursor from the previous page; empty for the first page.", Schema: map[string]any{"type": "string"}},
				{Name: "limit", In: "query", Schema: map[string]any{"type": "integer", "default": contentListDefaultLimit, "minimum": contentListMinLimit, "maximum": contentListMaxLimit}},
				sort, locale, fields},
			Responses: map[string]any{"200": ok200, "400": err400, "401": err401, "403": map[string]any{"description": "The sort names a field this caller may not read"}, "404": err404, "503": err503Locale},
		},
	}
	paths[base+"/stream"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "Stream changes to the content type", Description: contentStreamDescription(), Security: bearer,
			Parameters: []openAPIParameter{schema},
			Responses: map[string]any{
				"200": openAPIResponse{
					Description: "The stream, open until the client disconnects",
					Content:     map[string]openAPIMediaType{"text/event-stream": {Schema: contentStreamEventSchema()}},
				},
				"401": err401, "404": err404,
			},
		},
	}
	paths[base+"/bulk"] = openAPIPath{
		"post": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Create records in bulk", Security: bearer,
			Parameters:  []openAPIParameter{schema},
			RequestBody: &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}}},
			Responses:   map[string]any{"201": map[string]any{"description": "Created"}, "401": err401, "422": err422, "503": err503Rules},
		}, writers...),
	}
	paths[base+"/{id}"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "Get a record", Security: bearer,
			Parameters: []openAPIParameter{schema, id, populate, depth, locale, fields},
			Responses:  map[string]any{"200": ok200, "400": err400, "401": err401, "404": err404, "503": err503Locale},
		},
		"put": requiringRoles(&openAPIOperation{
			Tags: []string{tag}, Summary: "Update a record", Security: bearer,
			Parameters: []openAPIParameter{schema, id}, RequestBody: body,
			Responses: map[string]any{"200": ok200, "401": err401, "404": err404, "422": err422, "503": err503Rules},
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
			Responses:  map[string]any{"200": ok200, "401": err401, "404": err404, "422": err422, "503": err503Rules},
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

// engineOperation names one of the engine's own operations and states its
// effect.
type engineOperation struct {
	method, path, id string
	effect           core.Effect
}

// engineOperations is every operation the engine documents itself. Signing in
// counts as a write, because it starts a session. An audit entry or a rate
// limit counter a handler records on the side does not make it a write.
var engineOperations = []engineOperation{
	{"get", "/api/v1/content/{schema}", "listContent", core.EffectRead},
	{"post", "/api/v1/content/{schema}", "createContent", core.EffectWrite},
	{"get", "/api/v1/content/{schema}/cursor", "listContentByCursor", core.EffectRead},
	{"get", "/api/v1/content/{schema}/stream", "streamContentChanges", core.EffectRead},
	{"post", "/api/v1/content/{schema}/bulk", "createContentInBulk", core.EffectWrite},
	{"get", "/api/v1/content/{schema}/{id}", "getContent", core.EffectRead},
	{"put", "/api/v1/content/{schema}/{id}", "updateContent", core.EffectWrite},
	{"delete", "/api/v1/content/{schema}/{id}", "deleteContent", core.EffectDelete},
	{"get", "/api/v1/content/{schema}/{id}/relations/{field}", "listContentRelations", core.EffectRead},
	{"put", "/api/v1/content/{schema}/{id}/relations/{field}", "replaceContentRelations", core.EffectWrite},
	{"get", "/api/v1/content/{schema}/{id}/revisions", "listContentRevisions", core.EffectRead},
	{"put", "/api/v1/content/{schema}/{id}/revisions/{rev_id}/restore", "restoreContentRevision", core.EffectWrite},
	{"put", "/api/v1/content/{schema}/{id}/publish", "publishContent", core.EffectWrite},
	{"put", "/api/v1/content/{schema}/{id}/unpublish", "unpublishContent", core.EffectWrite},
	{"get", "/api/v1/delivery/{schema}", "listDeliveredContent", core.EffectRead},
	{"get", "/api/v1/delivery/{schema}/{id}", "getDeliveredContent", core.EffectRead},
	{"get", "/api/v1/schemas", "listContentTypes", core.EffectRead},
	{"get", "/api/v1/schemas/{name}", "getContentType", core.EffectRead},
	{"post", "/api/v1/auth/token", "createAccessToken", core.EffectWrite},
	{"get", "/api/v1/health", "getHealth", core.EffectRead},
	{"get", "/api/v1/ready", "getReadiness", core.EffectRead},
	{"get", "/.well-known/jwks.json", "getSigningKeys", core.EffectRead},
	{"get", "/robots.txt", "getRobotsTxt", core.EffectRead},

	{"get", "/api/admin/setup", "getSetupStatus", core.EffectRead},
	{"post", "/api/admin/setup", "completeSetup", core.EffectWrite},
	{"get", "/api/admin/auth/sign-in-routes", "listSignInRoutes", core.EffectRead},
	{"post", "/api/admin/auth/login", "signIn", core.EffectWrite},
	{"post", "/api/admin/auth/logout", "signOut", core.EffectWrite},
	{"post", "/api/admin/auth/mfa-verify", "verifyMFAChallenge", core.EffectWrite},
	{"post", "/api/admin/auth/refresh", "refreshSession", core.EffectWrite},
	{"get", "/api/admin/auth/me", "getCurrentUser", core.EffectRead},
	{"get", "/api/admin/auth/memberships", "listMemberships", core.EffectRead},
	{"get", "/api/admin/auth/provider-window", "getProviderWindow", core.EffectRead},
	{"put", "/api/admin/auth/provider-window", "setProviderWindow", core.EffectWrite},
	{"post", "/api/admin/auth/device", "startDeviceSignIn", core.EffectWrite},
	{"post", "/api/admin/auth/device/token", "exchangeDeviceCode", core.EffectWrite},
	{"get", "/api/admin/auth/device/{user_code}", "getDeviceSignIn", core.EffectRead},
	{"post", "/api/admin/auth/device/{user_code}/approve", "approveDeviceSignIn", core.EffectWrite},
	{"post", "/api/admin/auth/device/{user_code}/deny", "denyDeviceSignIn", core.EffectWrite},

	{"get", "/api/admin/users", "listUsers", core.EffectRead},
	{"post", "/api/admin/users", "createUser", core.EffectWrite},
	{"delete", "/api/admin/users/{id}", "deleteUser", core.EffectDelete},
	{"put", "/api/admin/users/{id}/password", "setUserPassword", core.EffectWrite},
	{"put", "/api/admin/users/{id}/roles", "setUserRoles", core.EffectWrite},
	{"put", "/api/admin/users/{id}/state", "setUserState", core.EffectWrite},
	{"get", "/api/admin/admin-tokens", "listAdminTokens", core.EffectRead},
	{"post", "/api/admin/admin-tokens", "createAdminToken", core.EffectWrite},
	{"get", "/api/admin/admin-tokens/grants", "listAdminTokenGrants", core.EffectRead},
	{"post", "/api/admin/admin-tokens/{id}/rotate", "rotateAdminToken", core.EffectWrite},
	{"delete", "/api/admin/admin-tokens/{id}", "revokeAdminToken", core.EffectDelete},
	{"get", "/api/admin/admin-tokens/{id}/requests", "listAdminTokenRequests", core.EffectRead},
	{"get", "/api/admin/api-key-scopes", "listApiKeyScopes", core.EffectRead},

	{"get", "/api/admin/schemas/export", "exportContentTypes", core.EffectRead},
	{"post", "/api/admin/schemas/import", "importContentTypes", core.EffectWrite},
	{"post", "/api/admin/gdpr/export", "exportSubjectData", core.EffectRead},
	{"post", "/api/admin/gdpr/erase", "eraseSubjectData", core.EffectDelete},
	{"post", "/api/admin/gdpr/subject-digests", "listSubjectDigests", core.EffectRead},

	{"get", "/api/admin/config", "getConfig", core.EffectRead},
	{"put", "/api/admin/config", "updateConfig", core.EffectWrite},
	{"get", "/api/admin/plugins/status", "listPluginStatus", core.EffectRead},
	{"get", "/api/admin/plugins/running", "listRunningPlugins", core.EffectRead},
	{"get", "/api/admin/plugins/{name}/config", "getPluginConfig", core.EffectRead},
	{"put", "/api/admin/plugins/{name}/config", "updatePluginConfig", core.EffectWrite},
	{"post", "/api/admin/plugins/{name}/config/reset", "resetPluginConfig", core.EffectDelete},
	{"get", "/api/admin/plugins/{name}/schema", "getPluginConfigSchema", core.EffectRead},
	{"get", "/api/admin/entitlements", "getEntitlements", core.EffectRead},
	{"get", "/api/admin/license", "getLicense", core.EffectRead},
	{"get", "/api/admin/tenant-features/{tenant}", "getTenantFeatures", core.EffectRead},
	{"put", "/api/admin/tenant-features/{tenant}", "setTenantFeatures", core.EffectWrite},
	{"get", "/api/admin/security/controls", "listSecurityControls", core.EffectRead},

	{"get", "/api/admin/health", "getAdminHealth", core.EffectRead},
	{"get", "/api/admin/ready", "getAdminReadiness", core.EffectRead},
	{"get", "/api/admin/pool/health", "getPoolHealth", core.EffectRead},
	{"get", "/api/admin/metrics", "getMetrics", core.EffectRead},
	{"get", "/api/admin/debug/goroutines", "listGoroutines", core.EffectRead},
	{"get", "/api/admin/openapi.json", "getOpenAPIDocument", core.EffectRead},
	{"get", "/api/admin/openapi/admin.json", "getAdminOpenAPIDocument", core.EffectRead},
	{"get", "/api/admin/openapi/public.json", "getPublicOpenAPIDocument", core.EffectRead},
}

// describeEngineOperations names the engine's operations and states their
// effects. An operation of a family this engine does not mount is skipped.
func describeEngineOperations(paths map[string]openAPIPath) {
	for _, e := range engineOperations {
		if op := paths[e.path][e.method]; op != nil {
			describing(op, e.id, e.effect)
		}
	}
}

// contentStreamEventNames are the names the change stream gives its events, in
// the order the handler subscribes to them.
func contentStreamEventNames() []string {
	names := make([]string, len(contentStreamEvents))
	for i, et := range contentStreamEvents {
		names[i] = string(et)
	}
	return names
}

// orList joins names as a sentence lists alternatives: "a, b or c".
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// contentStreamDescription says what the change stream sends, from the
// events, the comments and the heartbeat the handler writes, so the text
// cannot drift from the stream.
func contentStreamDescription() string {
	names := contentStreamEventNames()
	heartbeat := strconv.Itoa(int(contentStreamHeartbeat / time.Second))
	return "Server-Sent Events. While the stream is open it sends one event for each record created, updated or deleted in the content type, in the caller's tenant. " +
		"It replays nothing written before it opened.\n\n" +
		"Each event is a single `data` line holding a JSON object. `event` is " + orList(names) + ", `schema` names the content type, " +
		"and `data` is the record after the change, or as it was before a delete. " +
		"The record carries its `id` and leaves out every field a read withholds from the caller. " +
		"Relations are not populated and translations are not applied. A change to a draft reaches only a caller who may read drafts.\n\n" +
		"A change carries no `event` or `id` field, so an EventSource reports every change as a `message` event, and a client that reconnects cannot resume where it stopped. " +
		"A client that reads more slowly than changes arrive misses some of them.\n\n" +
		"When the caller's credential expires or is revoked, or the caller can no longer read the content type, the stream sends one last event named `" + contentStreamErrorEvent + "`, " +
		"whose `data` line holds `{\"error\": reason}`, and closes.\n\n" +
		"Comment lines carry no event. The stream opens with `" + fmt.Sprintf(contentStreamConnectedComment, "{schema}") + "` " +
		"and sends `" + contentStreamHeartbeatComment + "` every " + heartbeat + " seconds to keep an idle connection open. " +
		"The route takes no query parameters.\n\n" +
		"A record created in posts arrives as " +
		"`data: {\"data\":{\"id\":\"0f8fad5b-d9cb-469f-a165-70867728950e\",\"title\":\"Hello\"},\"event\":\"" + string(hooks.AfterCreate) + "\",\"schema\":\"posts\"}`."
}

// contentStreamEventSchema describes one event of the change stream.
//
// OpenAPI 3.1 has no field for the items of a stream, so the schema describes
// a single event, in the shape OpenAPI 3.2 gives an item of an event stream:
// an object whose data field is a string of JSON, with contentMediaType and
// contentSchema saying what that JSON holds.
func contentStreamEventSchema() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": "One event. A change sends only its data line, and comment lines are not events. The event that ends the stream also names itself.",
		"required":    []string{"data"},
		"properties": map[string]any{
			"event": map[string]any{
				"type":        "string",
				"enum":        []string{contentStreamErrorEvent},
				"description": "Absent on a change. Sent only on the last event, when the engine ends the stream.",
			},
			"data": map[string]any{
				"type":             "string",
				"contentMediaType": "application/json",
				"contentSchema":    map[string]any{"oneOf": []any{contentStreamChangeSchema(), contentStreamEndSchema()}},
			},
		},
	}
}

// contentStreamChangeSchema is the JSON a change's data line holds.
func contentStreamChangeSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"event", "schema", "data"},
		"properties": map[string]any{
			"event":  map[string]any{"type": "string", "enum": contentStreamEventNames()},
			"schema": map[string]any{"type": "string", "description": "The content type the record belongs to."},
			"data": map[string]any{
				"type":        "object",
				"description": "The record after the change, or as it was before a delete, without the fields a read withholds from the caller.",
				"required":    []string{"id"},
				"properties":  map[string]any{"id": map[string]any{"type": "string", "format": "uuid"}},
			},
		},
	}
}

// contentStreamEndSchema is the JSON the data line of the event that ends a
// stream holds.
func contentStreamEndSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"error"},
		"properties": map[string]any{
			"error": map[string]any{"type": "string", "description": "Why the stream stopped, for example that the credential expired or was revoked."},
		},
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
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403, "404": err404, "409": map[string]any{"description": "The account's erasure is not complete, so it stays locked"}},
	}, super...))
	set("/api/admin/users/{id}/password", "put", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Users"}, Summary: "Set a user's password; every session the account held ends", Security: both,
		Description: "remove_passkeys defaults to true, which removes the account's passkeys before the password changes when a running plugin supplies passkey removal. When they cannot be removed the answer is 503 and the password is not changed.",
		Parameters:  []openAPIParameter{id}, RequestBody: &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{"application/json": {Schema: map[string]any{"type": "object", "example": map[string]any{"password": "a-new-password-of-twelve", "remove_passkeys": true}}}}},
		Responses: map[string]any{"200": ok200, "401": err401, "403": err403, "404": err404, "409": map[string]any{"description": "The account signs in through a trusted issuer and has no password"}, "422": map[string]any{"description": "Password policy not met"}, "503": map[string]any{"description": "The passkeys could not be removed, so the password was not changed"}},
	}, super...))
	set("/api/admin/users/{id}", "delete", requiringRoles(&openAPIOperation{
		Tags: []string{"Admin / Users"}, Summary: "Delete a user", Security: both,
		Parameters: []openAPIParameter{id},
		Responses:  map[string]any{"204": map[string]any{"description": "Deleted"}, "401": err401, "403": err403, "404": err404, "409": map[string]any{"description": "The account's erasure is not complete, so it is kept"}},
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
	}, super...))
	set("/robots.txt", "get", &openAPIOperation{
		Tags: []string{"Admin / System"}, Summary: "Refuses every crawler", Description: "Every route on this server is authenticated, so there is nothing to index; the same refusal rides on every response as X-Robots-Tag.",
		Responses: map[string]any{"200": openAPIResponse{
			Description: "A robots.txt disallowing every path",
			Content:     map[string]openAPIMediaType{"text/plain": {Schema: map[string]any{"type": "string"}}},
		}},
	})
	set("/.well-known/jwks.json", "get", &openAPIOperation{
		Tags: []string{"Content API / Auth"}, Summary: "The signing keys, as a JWK Set", Description: "Verify a token the engine issued without calling it: every key the engine signs with, current and rotated, by kid.",
		Responses: map[string]any{"200": ok200},
	})
	set("/api/v1/schemas", "get", &openAPIOperation{
		Tags: []string{"Content API / Content Types"}, Summary: "List content types", Security: bearer,
		Responses: map[string]any{"200": ok200, "401": err401},
	})
	set("/api/v1/schemas/{name}", "get", &openAPIOperation{
		Tags: []string{"Content API / Content Types"}, Summary: "Get a content type's definition", Security: bearer,
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
	contested := contestedOperationIDs(paths, plugins, owners)
	documentDeclaredRoutes(paths, plugins, owners, ungated, contested, true)
	documentDeclaredRoutes(paths, plugins, owners, ungated, contested, false)
}

// contestedOperationIDs returns the operation ids no declared route may
// carry: each id an operation in paths already holds, and each id more than
// one declared route names. Plugins start in an order that can change between
// boots, so an id that went to whichever route came first would move, and a
// client generated from one boot's document would break on the next.
func contestedOperationIDs(paths map[string]openAPIPath, sets ...[]plugin.PluginRoutes) map[string]bool {
	contested := map[string]bool{}
	for _, methods := range paths {
		for _, op := range methods {
			if op != nil && op.OperationID != "" {
				contested[op.OperationID] = true
			}
		}
	}
	named := map[string]bool{}
	for _, set := range sets {
		for _, owner := range set {
			for _, r := range owner.Routes {
				if r.Doc == nil || r.Doc.OperationID == "" {
					continue
				}
				if named[r.Doc.OperationID] {
					contested[r.Doc.OperationID] = true
				}
				named[r.Doc.OperationID] = true
			}
		}
	}
	return contested
}

// documentDeclaredRoutes writes the declared routes that carry a Doc, or the
// ones that carry none.
func documentDeclaredRoutes(paths map[string]openAPIPath, plugins, owners []plugin.PluginRoutes, ungated func(string) bool, contested map[string]bool, withDoc bool) {
	for _, p := range plugins {
		feature := ""
		if ungated == nil || !ungated(p.Name) {
			feature = p.Name
		}
		documentRouteSet(paths, p.Routes, "Plugin / "+p.Name, feature, contested, withDoc)
	}
	for _, o := range owners {
		documentRouteSet(paths, o.Routes, o.Name, "", contested, withDoc)
	}
}

// documentRouteSet writes one owner's routes, those that carry a Doc or those
// that carry none, under tag and feature. A route naming a contested id is
// written without it.
func documentRouteSet(paths map[string]openAPIPath, routes []core.RouteDecl, tag, feature string, contested map[string]bool, withDoc bool) {
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
		op := declaredRouteOperation(r, tag, feature)
		if contested[op.OperationID] {
			op.OperationID = ""
		}
		paths[r.Pattern][method] = op
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
	if d.RequestExample != nil || d.RequestSchema != nil {
		schema := map[string]any{"type": "object"}
		if d.RequestSchema != nil {
			schema = maps.Clone(d.RequestSchema)
		}
		if d.RequestExample != nil {
			schema["example"] = d.RequestExample
		}
		op.RequestBody = &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{
			"application/json": {Schema: schema},
		}}
	}
	describing(op, d.OperationID, d.Effect)
	if len(d.Responses) > 0 {
		op.Responses = make(map[string]any, len(d.Responses))
		for status, description := range d.Responses {
			op.Responses[strconv.Itoa(status)] = map[string]any{"description": description}
		}
	}
	documentResponseMediaTypes(op, d.ResponseMediaTypes)
}

// documentResponseMediaTypes writes the media types a Doc names as the
// content of each success response that carries a body, every 2xx but 204
// and 205. An error is the engine's JSON envelope whatever the route serves,
// so no other response takes them. A type ParseResponseMediaType refuses is
// left out rather than published, and with none left the responses stay as
// they were.
func documentResponseMediaTypes(op *openAPIOperation, types []core.RouteMediaType) {
	content := map[string]openAPIMediaType{}
	for _, mt := range types {
		if _, ok := core.ParseResponseMediaType(mt.MediaType); ok {
			content[mt.MediaType] = openAPIMediaType{Schema: mt.Schema}
		}
	}
	if len(content) == 0 {
		return
	}
	for status, response := range op.Responses {
		code, err := strconv.Atoi(status)
		if err != nil || code < 200 || code > 299 || code == http.StatusNoContent || code == http.StatusResetContent {
			continue
		}
		description := ""
		if r, ok := response.(map[string]any); ok {
			description, _ = r["description"].(string)
		}
		op.Responses[status] = openAPIResponse{Description: description, Content: content}
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

// documentDelivery writes the two public delivery reads. They take no
// credential, so they carry no security requirement, and a request that
// sends one is refused.
func documentDelivery(paths map[string]openAPIPath) {
	const tag = "Content API / Delivery"
	schema := openAPIParameter{Name: "schema", In: "path", Required: true, Description: "A content type its tenant marked for public delivery, spelled as it is stored.", Schema: map[string]any{"type": "string"}}
	id := openAPIParameter{Name: "id", In: "path", Required: true, Description: "Record id, in lower case with hyphens. Any other spelling answers 404.", Schema: map[string]any{"type": "string", "format": "uuid"}}
	populate := openAPIParameter{Name: "populate", In: "query", Description: "Relation fields to resolve inline, comma-separated, at most 8, each a path of at most 2 dot-separated segments. Each segment names a belongs_to relation into a marked content type, and anything else, a wildcard or a field mask included, is refused. A related record that is not published reads as null.", Schema: map[string]any{"type": "string"}}
	depth := openAPIParameter{Name: "depth", In: "query", Description: "How many levels of belongs_to relations into marked content types to resolve. Other relations are not resolved.", Schema: map[string]any{"type": "integer", "minimum": 0, "maximum": deliveryMaxDepth}}
	fields := openAPIParameter{Name: "fields", In: "query", Description: "The fields of each record's data to return, comma-separated. A relation field keeps both its stored column and its populated record. Every record carries id, created_at and updated_at, so the list may not name them. Each name appears once, with no spaces around it. A field marked private, a relation into a content type that is not marked and a field the content type does not have are refused alike.", Schema: map[string]any{"type": "string"}}
	const served = "Published records only, without the fields marked private and the relations into content types that are not marked. Served public with s-maxage and a strong ETag, so a CDN may keep the answer. The tenant comes from the host. A parameter outside the ones listed, or one sent twice, is refused."
	responses := map[string]any{
		"200": map[string]any{"description": "OK"},
		"304": map[string]any{"description": "Not modified"},
		"400": map[string]any{"description": "A parameter outside the grammar, or a request that carries a credential"},
		"404": map[string]any{"description": "No such record, or a content type that is not marked for public delivery"},
		"429": map[string]any{"description": "Too many requests from this address"},
		"503": map[string]any{"description": "The content type's definition or a related record could not be read"},
	}
	paths["/api/v1/delivery/{schema}"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "List published records", Description: served + " A list takes up to 4 filters, each value at most 255 characters and in the form of its field: exact matches, written filters[field]=value, on id and on the text, email, URL, uid, relation, integer, decimal, boolean, date and datetime fields served here, and inclusive bounds on created_at, written filters[created_at][gte]=value and filters[created_at][lte]=value.",
			Parameters: []openAPIParameter{schema,
				{Name: "limit", In: "query", Description: "Records per page.", Schema: map[string]any{"type": "integer", "default": contentListDefaultLimit, "minimum": contentListMinLimit, "maximum": contentListMaxLimit}},
				{Name: "offset", In: "query", Schema: map[string]any{"type": "integer", "default": 0, "minimum": 0, "maximum": deliveryMaxOffset}},
				{Name: "sort", In: "query", Description: "The order of the list: sort=created_at for oldest first, or sort=-created_at for newest first, which is also the order without it. Records created at the same moment follow their id. Any other field answers 400.", Schema: map[string]any{"type": "string"}},
				populate, depth, fields},
			Responses: responses,
		},
	}
	paths["/api/v1/delivery/{schema}/{id}"] = openAPIPath{
		"get": {
			Tags: []string{tag}, Summary: "Get a published record", Description: served,
			Parameters: []openAPIParameter{schema, id, populate, depth, fields},
			Responses:  responses,
		},
	}
}
