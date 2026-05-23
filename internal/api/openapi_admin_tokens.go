package api

import (
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// xAdminGrant is the admin token grant an operation declares. An operation
// carrying it lists the adminToken scheme beside the session ones. One
// without it cannot be called with an admin token.
const xAdminGrant = "x-admin-grant"

// adminTokenScheme describes the admin token to a client: a bearer
// credential opening with lyat_.
var adminTokenScheme = map[string]any{
	"type":         "http",
	"scheme":       "bearer",
	"bearerFormat": "lyat",
	"description":  "An admin token, sent as Authorization: Bearer lyat_<secret>. It reaches only the admin operations whose x-admin-grant it holds, in the one tenant it was issued for.",
}

// annotateAdminGrants marks every operation a grant opens to admin tokens:
// the engine's own table and each plugin declaration under /api/admin. A
// route a declaration marks session only is left unmarked, as one the
// engine's patterns cover is.
func annotateAdminGrants(paths map[string]openAPIPath, plugins []plugin.PluginRoutes) {
	declared := declaredRoutes(plugins)
	mark := func(rd core.RouteDecl) {
		if rd.AdminGrant == "" || !core.IsAdminGrant(rd.AdminGrant) || core.IsSessionOnlyAdminRoute(rd.Method, rd.Pattern, declared...) {
			return
		}
		op := paths[rd.Pattern][strings.ToLower(rd.Method)]
		if op == nil || op.Extensions[xAdminGrant] != nil {
			return
		}
		// A fresh slice: the session schemes are shared between operations.
		sec := make([]map[string][]any, 0, len(op.Security)+1)
		sec = append(sec, op.Security...)
		sec = append(sec, map[string][]any{"adminToken": {}})
		op.Security = sec
		if op.Extensions == nil {
			op.Extensions = map[string]any{}
		}
		op.Extensions[xAdminGrant] = rd.AdminGrant
	}
	for _, rd := range engineAdminGrants {
		mark(rd)
	}
	for _, p := range plugins {
		for _, rd := range p.Routes {
			if strings.HasPrefix(rd.Pattern, adminPathPrefix+"/") {
				mark(rd)
			}
		}
	}
}

// documentAdminTokens writes the six admin token routes. All are session
// only and need admin or super_admin.
func documentAdminTokens(paths map[string]openAPIPath) {
	both := []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}
	admins := []string{"admin", "super_admin"}
	tag := []string{"Admin / Admin tokens"}
	id := openAPIParameter{Name: "id", In: "path", Required: true, Schema: map[string]any{"type": "string", "format": "uuid"}}
	limit := openAPIParameter{Name: "limit", In: "query", Schema: map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}}
	offset := openAPIParameter{Name: "offset", In: "query", Schema: map[string]any{"type": "integer", "minimum": 0, "default": 0}}
	resp := func(codes ...string) map[string]any {
		all := map[string]any{
			"200": map[string]any{"description": "OK"},
			"201": map[string]any{"description": "Created; the token is in the body and nowhere else"},
			"204": map[string]any{"description": "Revoked"},
			"400": map[string]any{"description": "Malformed request"},
			"401": map[string]any{"description": "Unauthorized"},
			"403": map[string]any{"description": "Forbidden: not a signed-in session, not the owner, or the MFA code or password was missing or wrong"},
			"404": map[string]any{"description": "Not found"},
			"409": map[string]any{"description": "The token is revoked or expired"},
			"422": map[string]any{"description": "Validation failed"},
		}
		out := map[string]any{}
		for _, c := range codes {
			out[c] = all[c]
		}
		return out
	}
	body := func(example map[string]any) *openAPIRequestBody {
		return &openAPIRequestBody{Required: true, Content: map[string]openAPIMediaType{
			"application/json": {Schema: map[string]any{"type": "object", "example": example}},
		}}
	}
	set := func(path, method string, op *openAPIOperation) {
		if paths[path] == nil {
			paths[path] = openAPIPath{}
		}
		if paths[path][method] == nil {
			paths[path][method] = requiringRoles(op, admins...)
		}
	}

	set("/api/admin/admin-tokens", "get", &openAPIOperation{
		Tags: tag, Summary: "List admin tokens", Security: both,
		Description: "The tenant's admin tokens, newest first: every one for a super admin, the caller's own for an admin. Each carries its owner's email, grants, address list, expiry, last use, revocation and display prefix, never the token or its hash.",
		Parameters:  []openAPIParameter{limit, offset},
		Responses:   resp("200", "400", "401", "403"),
	})
	set("/api/admin/admin-tokens", "post", &openAPIOperation{
		Tags: tag, Summary: "Issue an admin token", Security: both,
		Description: "Issues a token owned by the caller and bound to one tenant: the caller's, or for a super admin the named tenant_id. expires_at is required and at most 90 days away; allowed_ips takes addresses and CIDR ranges. An account with MFA enrolled confirms with mfa_code, any other with password. The token is returned once.",
		RequestBody: body(map[string]any{"name": "ci-deploy", "grants": []string{"schemas:read"}, "expires_at": "2026-12-01T00:00:00Z", "allowed_ips": []string{"203.0.113.0/24"}, "password": "your-password"}),
		Responses:   resp("201", "400", "401", "403", "422"),
	})
	set("/api/admin/admin-tokens/grants", "get", &openAPIOperation{
		Tags: tag, Summary: "The admin grant catalog", Security: both,
		Description: "Every grant a token can hold, with its description and the method and pattern of each route that declares it.",
		Responses:   resp("200", "401", "403"),
	})
	set("/api/admin/admin-tokens/{id}/rotate", "post", &openAPIOperation{
		Tags: tag, Summary: "Rotate an admin token", Security: both,
		Description: "Issues a successor with the same name, grants, tenant and address list and a new expiry, at most 90 days away. The old token keeps working for at most seven more days. Owner only, with the same confirmation as issuing.",
		Parameters:  []openAPIParameter{id},
		RequestBody: body(map[string]any{"expires_at": "2027-01-01T00:00:00Z", "mfa_code": "123456"}),
		Responses:   resp("201", "400", "401", "403", "404", "409", "422"),
	})
	set("/api/admin/admin-tokens/{id}", "delete", &openAPIOperation{
		Tags: tag, Summary: "Revoke an admin token", Security: both,
		Description: "Ends the token now. Its owner or a super admin of the tenant may.",
		Parameters:  []openAPIParameter{id},
		Responses:   resp("204", "401", "403", "404", "409"),
	})
	set("/api/admin/admin-tokens/{id}/requests", "get", &openAPIOperation{
		Tags: tag, Summary: "An admin token's request log", Security: both,
		Description: "One row per request the token made on the admin API, refusals included, newest first: method, route pattern, status, client address and time. Kept for 90 days.",
		Parameters:  []openAPIParameter{id, limit, offset},
		Responses:   resp("200", "400", "401", "403", "404"),
	})
}
