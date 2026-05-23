package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// registerReportsAndVaults registers two kinds the way two plugins would, for
// one test. Both admit admin by default and take an action beyond the four
// every resource takes.
func registerReportsAndVaults(t *testing.T) {
	t.Helper()
	keepPermissionKinds(t)
	RegisterPermissionKind(PermissionKind{
		General:        "reports",
		Prefix:         "report:",
		ValidSlug:      testSlugRe.MatchString,
		Actions:        []string{"export"},
		AdminByDefault: true,
	})
	RegisterPermissionKind(PermissionKind{
		General:        "vaults",
		Prefix:         "vault:",
		ValidSlug:      testSlugRe.MatchString,
		Actions:        []string{"export"},
		AdminByDefault: true,
	})
}

// A resource is a schema name, the wildcard, every resource of a registered
// kind, or one of them. The slug after a kind's prefix is held to the kind's
// own rule, so a rule cannot be written for a resource that can never exist
// and then match nothing.
func TestValidatePermissionResource_Shapes(t *testing.T) {
	registerReportsAndVaults(t)
	for _, ok := range []string{"*", "articles", "reports", "report:weekly", "report:a", "report:sync-2_x", "vaults", "vault:main"} {
		assert.True(t, ValidatePermissionResource(ok), ok)
	}
	for _, bad := range []string{"", "report:", "report:Weekly", "report:-x", "report:1x", "report:has space", "report:" + strings.Repeat("a", 41),
		"vault:", "vault:Main"} {
		assert.False(t, ValidatePermissionResource(bad), "%q", bad)
	}
}

// With no kind registered every resource reads as a schema, so a build that
// links no plugin owning a kind gives no resource the treatment of one.
func TestValidatePermissionResource_NoKindReadsEverythingAsASchema(t *testing.T) {
	keepPermissionKinds(t)
	permissionKinds.mu.Lock()
	permissionKinds.kinds = nil
	permissionKinds.mu.Unlock()

	for _, res := range []string{"reports", "report:weekly", "vault:main"} {
		assert.True(t, ValidatePermissionResource(res), res)
		assert.False(t, IsPluginPermissionResource(res), res)
		assert.Equal(t, PermissionResourceAll, GeneralPermissionResource(res), res)
		assert.Equal(t, []string{"create", "read", "update", "delete"}, PermissionActionsFor(res), res)
	}
	assert.False(t, ValidatePermissionResource(""))
}

// Two registered kinds never mistake each other's resources: a rule for one
// report's slug says nothing about the vault with the same slug.
func TestIsPluginPermissionResource_KindsStayApart(t *testing.T) {
	registerReportsAndVaults(t)
	for _, res := range []string{"reports", "report:weekly", "vaults", "vault:weekly"} {
		assert.True(t, IsPluginPermissionResource(res), res)
	}
	for _, res := range []string{"*", "articles", "report", "vault", "report:", "vault:Weekly"} {
		assert.False(t, IsPluginPermissionResource(res), res)
	}
	assert.Equal(t, "reports", GeneralPermissionResource("report:weekly"))
	assert.Equal(t, "vaults", GeneralPermissionResource("vault:weekly"))
}

// The general row beside a resource's own is the wildcard for a schema and
// the kind's own general resource for a resource of a kind, which is its own
// general row, so the wildcard never reaches a plugin resource.
func TestGeneralPermissionResource(t *testing.T) {
	registerReportsAndVaults(t)
	assert.Equal(t, "*", GeneralPermissionResource("articles"))
	assert.Equal(t, "*", GeneralPermissionResource("*"))
	assert.Equal(t, "reports", GeneralPermissionResource("report:weekly"))
	assert.Equal(t, "reports", GeneralPermissionResource("reports"))
	assert.Equal(t, "vaults", GeneralPermissionResource("vault:main"))
	assert.Equal(t, "vaults", GeneralPermissionResource("vaults"))
}

// An action a kind adds is one a schema does not take, so it is refused on
// every resource outside the kind, the wildcard included.
func TestValidPermissionAction_AKindActionIsForItsResourcesOnly(t *testing.T) {
	registerReportsAndVaults(t)
	for _, res := range []string{"reports", "report:weekly", "vaults", "vault:main"} {
		for _, a := range []string{"create", "read", "update", "delete", "export"} {
			assert.True(t, ValidPermissionAction(res, a), "%s on %s", a, res)
		}
	}
	for _, res := range []string{"*", "articles"} {
		for _, a := range []string{"create", "read", "update", "delete"} {
			assert.True(t, ValidPermissionAction(res, a), "%s on %s", a, res)
		}
		assert.False(t, ValidPermissionAction(res, "export"), res)
	}
	assert.False(t, ValidPermissionAction("reports", "publish"))
	assert.False(t, ValidPermissionAction("vaults", "approve"))
	assert.Equal(t, []string{"create", "read", "update", "delete", "export"}, PermissionActionsFor("reports"))
	assert.Equal(t, []string{"create", "read", "update", "delete", "export"}, PermissionActionsFor("vault:main"))
	assert.Equal(t, []string{"create", "read", "update", "delete"}, PermissionActionsFor("articles"))
}
