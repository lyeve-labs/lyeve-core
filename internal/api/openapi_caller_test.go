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
