package core

import (
	"context"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keepPermissionKinds restores the registry when the test ends, so a kind a
// test registers never reaches the next test.
func keepPermissionKinds(t *testing.T) {
	t.Helper()
	saved := registeredPermissionKinds()
	t.Cleanup(func() {
		permissionKinds.mu.Lock()
		permissionKinds.kinds = saved
		permissionKinds.mu.Unlock()
	})
}

// testSlugRe is a slug rule of the shape plugins give their kinds.
var testSlugRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)

// The kernel registers no kind of its own, so every kind a build knows came
// from a plugin it links.
func TestPermissionKinds_KernelRegistersNone(t *testing.T) {
	assert.Empty(t, PermissionKinds())
}

// A kind a plugin registers is validated, answered and admitted with its own
// slug rule and actions, and listed beside the others by General.
func TestRegisterPermissionKind_NewKindIsGovernedLikeTheOthers(t *testing.T) {
	keepPermissionKinds(t)
	RegisterPermissionKind(PermissionKind{
		General:   "vaults",
		Prefix:    "vault:",
		ValidSlug: testSlugRe.MatchString,
		Actions:   []string{"seal"},
	})
	RegisterPermissionKind(PermissionKind{
		General:        "reports",
		Prefix:         "report:",
		ValidSlug:      func(s string) bool { return s == "weekly" || s == "daily" },
		Actions:        []string{"export"},
		AdminByDefault: true,
	})

	assert.True(t, ValidatePermissionResource("reports"))
	assert.True(t, ValidatePermissionResource("report:weekly"))
	assert.False(t, ValidatePermissionResource("report:"), "a prefix with no slug")
	assert.False(t, ValidatePermissionResource("report:monthly"), "a slug the kind refuses")

	assert.True(t, IsPluginPermissionResource("report:daily"))
	assert.Equal(t, "reports", GeneralPermissionResource("report:daily"))
	assert.Equal(t, "reports", GeneralPermissionResource("reports"))
	assert.Equal(t, []string{"create", "read", "update", "delete", "export"}, PermissionActionsFor("report:weekly"))
	assert.True(t, ValidPermissionAction("reports", "export"))
	assert.False(t, ValidPermissionAction("reports", "seal"), "another kind's action")
	assert.False(t, ValidPermissionAction("articles", "export"), "a schema takes no kind's action")

	ok, err := RoleOnlyPermissionChecker{}.Allowed(context.Background(), []string{RoleAdmin}, "report:weekly", "export")
	require.NoError(t, err)
	assert.True(t, ok, "admin is allowed a kind registered with AdminByDefault")

	names := make([]string, 0, 2)
	for _, k := range PermissionKinds() {
		names = append(names, k.General)
	}
	assert.Equal(t, []string{"reports", "vaults"}, names, "kinds are listed by General")
}

// AdminByDefault is the only thing that admits admin to a kind with no rule.
// A kind registered without it gives admin what it gives any other role.
func TestRegisterPermissionKind_AdminOnlyByDefaultWhenAsked(t *testing.T) {
	keepPermissionKinds(t)
	RegisterPermissionKind(PermissionKind{
		General:   "vaults",
		Prefix:    "vault:",
		ValidSlug: testSlugRe.MatchString,
	})

	checker := RoleOnlyPermissionChecker{}
	ok, err := checker.Allowed(context.Background(), []string{RoleAdmin}, "vault:main", "read")
	require.NoError(t, err)
	assert.False(t, ok, "admin is refused a kind registered without AdminByDefault")

	ok, err = checker.Allowed(context.Background(), []string{RoleSuperAdmin}, "vault:main", "read")
	require.NoError(t, err)
	assert.True(t, ok, "super_admin passes every gate")
	assert.Equal(t, []string{"create", "read", "update", "delete"}, PermissionActionsFor("vaults"))
}

// A kind registered again under its General replaces the one registered
// before, and its slug rule is the one read from then on.
func TestRegisterPermissionKind_ReplacesAKindOfTheSameName(t *testing.T) {
	keepPermissionKinds(t)
	RegisterPermissionKind(PermissionKind{
		General:   "reports",
		Prefix:    "report:",
		ValidSlug: testSlugRe.MatchString,
		Actions:   []string{"export"},
	})
	require.True(t, ValidatePermissionResource("report:other"))

	RegisterPermissionKind(PermissionKind{
		General:        "reports",
		Prefix:         "report:",
		ValidSlug:      func(s string) bool { return s == "weekly" },
		Actions:        []string{"export"},
		AdminByDefault: true,
	})

	assert.Len(t, PermissionKinds(), 1, "the kind was replaced, not added")
	assert.True(t, ValidatePermissionResource("report:weekly"))
	assert.False(t, ValidatePermissionResource("report:other"), "the later slug rule is the one read")
}

// A kind that cannot work stops the plugin that registers it at init, and
// leaves the registry as it was.
func TestRegisterPermissionKind_RefusesAKindThatCannotWork(t *testing.T) {
	keepPermissionKinds(t)
	valid := func(string) bool { return true }
	RegisterPermissionKind(PermissionKind{General: "reports", Prefix: "report:", ValidSlug: valid})
	cases := map[string]PermissionKind{
		"empty general":             {General: "", Prefix: "x:", ValidSlug: valid},
		"wildcard general":          {General: "*", Prefix: "x:", ValidSlug: valid},
		"general with colon":        {General: "x:y", Prefix: "x:", ValidSlug: valid},
		"empty prefix":              {General: "xs", Prefix: "", ValidSlug: valid},
		"bare colon prefix":         {General: "xs", Prefix: ":", ValidSlug: valid},
		"prefix with no colon":      {General: "xs", Prefix: "x", ValidSlug: valid},
		"no slug rule":              {General: "xs", Prefix: "x:"},
		"empty action":              {General: "xs", Prefix: "x:", ValidSlug: valid, Actions: []string{""}},
		"prefix another kind holds": {General: "reportish", Prefix: "report:", ValidSlug: valid},
		"prefix under another kind": {General: "subreports", Prefix: "report:sub:", ValidSlug: valid},
	}
	for name, k := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Panics(t, func() { RegisterPermissionKind(k) })
			assert.Len(t, PermissionKinds(), 1)
		})
	}
}

// Two prefixes where one begins the other would let one resource name belong
// to both kinds, so the shorter one is refused as well as the longer one.
func TestRegisterPermissionKind_RefusesAPrefixOverAnotherKind(t *testing.T) {
	keepPermissionKinds(t)
	RegisterPermissionKind(PermissionKind{General: "deep", Prefix: "deep:one:", ValidSlug: testSlugRe.MatchString})

	assert.Panics(t, func() {
		RegisterPermissionKind(PermissionKind{General: "deeps", Prefix: "deep:", ValidSlug: testSlugRe.MatchString})
	})
	assert.Len(t, PermissionKinds(), 1)
	assert.Equal(t, "deep", GeneralPermissionResource("deep:one:x"))
	assert.Equal(t, PermissionResourceAll, GeneralPermissionResource("deep:x"))
}

// The list a caller gets is its own, so editing it cannot widen what a rule
// on a kind may grant.
func TestPermissionKinds_ReturnsCopies(t *testing.T) {
	registerReportsAndVaults(t)
	kinds := PermissionKinds()
	kinds[0].Actions[0] = "delete_everything"
	kinds[0].General = "changed"

	again := PermissionKinds()
	assert.Equal(t, "reports", again[0].General)
	assert.Equal(t, []string{"export"}, again[0].Actions)
	assert.Equal(t, []string{"create", "read", "update", "delete", "export"}, PermissionActionsFor("reports"))
}

// The role-only checker reads the registry: super_admin everywhere, admin on
// every resource of a kind registered with AdminByDefault and nowhere else,
// and every other role nowhere.
func TestRoleOnlyPermissionChecker_AnswersFromTheRegisteredKinds(t *testing.T) {
	registerReportsAndVaults(t)
	adminResources := map[string]bool{
		"reports": true, "report:weekly": true, "vaults": true, "vault:main": true,
		"*": false, "articles": false, "report": false, "report:": false, "report:Bad": false,
		"vault": false, "vault:": false, "vault:-x": false,
	}
	checker := RoleOnlyPermissionChecker{}
	ctx := context.Background()
	for resource, adminAllowed := range adminResources {
		for _, action := range []string{"create", "read", "update", "delete", "export"} {
			ok, err := checker.Allowed(ctx, []string{RoleSuperAdmin}, resource, action)
			require.NoError(t, err)
			assert.True(t, ok, "super_admin %s on %q", action, resource)

			ok, err = checker.Allowed(ctx, []string{RoleAdmin}, resource, action)
			require.NoError(t, err)
			assert.Equal(t, adminAllowed, ok, "admin %s on %q", action, resource)

			ok, err = checker.Allowed(ctx, []string{"editor", "viewer"}, resource, action)
			require.NoError(t, err)
			assert.False(t, ok, "editor %s on %q", action, resource)

			ok, err = checker.Allowed(ctx, nil, resource, action)
			require.NoError(t, err)
			assert.False(t, ok, "no roles %s on %q", action, resource)
		}
	}
}
