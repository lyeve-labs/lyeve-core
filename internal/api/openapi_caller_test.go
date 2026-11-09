package api

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// roleFreeAdminOperations are the admin operations that answer before sign-in
// or answer every signed-in role, so no role is theirs to name.
var roleFreeAdminOperations = []string{
	"GET /api/admin/auth/me",
	"GET /api/admin/auth/memberships",
	"GET /api/admin/auth/sign-in-routes",
	"GET /api/admin/health",
	"GET /api/admin/license",
	"GET /api/admin/plugins/running",
	"GET /api/admin/ready",
	"GET /api/admin/setup",
	"POST /api/admin/auth/device",
	"POST /api/admin/auth/device/token",
	"POST /api/admin/auth/login",
	"POST /api/admin/auth/logout",
	"POST /api/admin/auth/mfa-verify",
	"POST /api/admin/auth/refresh",
	"POST /api/admin/setup",
}

// Every other admin operation names the roles that may call it, so a client
// can tell who may call it before it tries. An operation a role guards but
// whose document names no role reads as open to every signed-in caller.
func TestOpenAPI_EveryAdminOperationNamesItsRoles(t *testing.T) {
	var missing []string
	for path, methods := range engineDocWithEverything().Paths {
		if !isAdminPath(path) {
			continue
		}
		for method, op := range methods {
			if roles, _ := op.Extensions[xRoles].([]string); len(roles) == 0 {
				missing = append(missing, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(missing)
	assert.Equal(t, roleFreeAdminOperations, missing)
}

// A caller below super admin reads every route outside /api/admin and the
// admin operations its role may call, and never one only a super admin may.
func TestOpenAPIForCaller_KeepsTheAdminOperationsTheRoleMayCall(t *testing.T) {
	doc := openAPIForCaller(engineDocWithEverything(), func(role string) bool { return role == "admin" })

	for path, methods := range doc.Paths {
		if !isAdminPath(path) {
			continue
		}
		for method, op := range methods {
			roles, _ := op.Extensions[xRoles].([]string)
			if len(roles) > 0 {
				assert.Contains(t, roles, "admin", "%s %s", strings.ToUpper(method), path)
			}
		}
	}
	assert.NotContains(t, doc.Paths["/api/admin/users"], "get", "listing every user is for a super admin")
	assert.NotContains(t, doc.Paths, "/api/admin/tenant-features/{tenant}")
	assert.Contains(t, doc.Paths["/api/admin/auth/provider-window"], "get")
	assert.Contains(t, doc.Paths["/api/admin/auth/me"], "get", "an operation every signed-in role may call stays")
	assert.Contains(t, doc.Paths, "/.well-known/jwks.json", "every route outside /api/admin stays")
}

// A role that no admin operation names reads none of the guarded ones. The
// routes outside /api/admin stay whole, as the public half has them.
func TestOpenAPIForCaller_ARoleNothingNamesReadsNoGuardedOperation(t *testing.T) {
	doc := openAPIForCaller(engineDocWithEverything(), func(string) bool { return false })
	for path, methods := range doc.Paths {
		if !isAdminPath(path) {
			continue
		}
		for method, op := range methods {
			roles, _ := op.Extensions[xRoles].([]string)
			assert.Empty(t, roles, "%s %s", strings.ToUpper(method), path)
		}
	}
}
