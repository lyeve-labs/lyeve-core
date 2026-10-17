package enginehost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// allowEverything answers yes to every question, which is the checker a
// plugin that wanted every request let through would register.
type allowEverything struct{}

func (allowEverything) Allowed(context.Context, []string, string, string) (bool, error) {
	return true, nil
}

// ruleChecker stands in for the checker the permissions plugin registers.
type ruleChecker struct{ rules int }

func (*ruleChecker) Allowed(context.Context, []string, string, string) (bool, error) {
	return false, nil
}

// Each role's value below is a distinct pointer the host can be asked about.
// The embedded interface is never called.
type (
	roleEmailSender     struct{ core.EmailSender }
	roleFlowValidator   struct{ core.FlowDefinitionValidator }
	roleFlowInvoker     struct{ core.FlowInvoker }
	roleLocalizer       struct{ core.ContentLocalizer }
	roleEntryWriter     struct{ core.ContentEntryWriter }
	roleRevisionStore   struct{ core.RecordRevisionStore }
	roleStorage         struct{ core.Storage }
	roleUserProvisioner struct{ core.UserProvisioner }
)

// ordinaryGrant is what most plugins of the build hold: their own tables,
// their routes and the hook bus, and no role.
const ordinaryGrant = core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapHooks

func TestRoleRegistrars_APluginWithoutTheGrantCannotReplaceThePermissionChecker(t *testing.T) {
	h := &engineHost{}
	owner := core.NewScopedHost(h, "permissions", ordinaryGrant|core.CapPermissionChecker)
	intruder := core.NewScopedHost(h, "ab-testing", ordinaryGrant)

	rules := &ruleChecker{}
	require.NoError(t, owner.RegisterPermissionChecker(rules))

	err := intruder.RegisterPermissionChecker(allowEverything{})
	require.ErrorIs(t, err, core.ErrCapDenied)
	assert.ErrorContains(t, err, "CapPermissionChecker", "the refusal names the grant the plugin lacks")
	assert.Same(t, rules, h.PermissionChecker(), "a plugin without the grant replaced the rules")

	require.ErrorIs(t, intruder.RegisterPermissionChecker(nil), core.ErrCapDenied)
	assert.Same(t, rules, h.PermissionChecker(), "a plugin without the grant dropped the install to role-only checks")
}

func TestRoleRegistrars_APluginWithoutTheGrantCannotReplaceTheSchemaEngine(t *testing.T) {
	h := &engineHost{}
	owner := core.NewScopedHost(h, "schema", ordinaryGrant|core.CapSchema)
	intruder := core.NewScopedHost(h, "ab-testing", ordinaryGrant)

	engine := schemaengine.New(nil)
	require.NoError(t, owner.RegisterSchemaEngine(engine))

	require.ErrorIs(t, intruder.RegisterSchemaEngine(schemaengine.New(nil)), core.ErrCapDenied)
	assert.Same(t, engine, h.Schema(), "a plugin without the grant replaced the schema engine")

	require.ErrorIs(t, intruder.RegisterSchemaEngine(nil), core.ErrCapDenied)
	assert.Same(t, engine, h.Schema(), "a plugin without the grant cleared the schema engine")
}

// content and bulk-import hold CapSchema for their own DDL, so the grant alone
// does not stop them. The record of who holds the engine does.
func TestRoleRegistrars_APluginWithTheGrantCannotTakeTheRoleAnotherHolds(t *testing.T) {
	h := &engineHost{}
	schema := core.NewScopedHost(h, "schema", ordinaryGrant|core.CapSchema)
	content := core.NewScopedHost(h, "content", ordinaryGrant|core.CapSchema)

	engine := schemaengine.New(nil)
	require.NoError(t, schema.RegisterSchemaEngine(engine))

	err := content.RegisterSchemaEngine(schemaengine.New(nil))
	require.ErrorIs(t, err, core.ErrHostRoleHeld)
	assert.ErrorContains(t, err, "schema holds the schema engine", "the refusal names the holder")
	require.ErrorIs(t, content.RegisterSchemaEngine(nil), core.ErrHostRoleHeld)
	assert.Same(t, engine, h.Schema())

	require.NoError(t, schema.RegisterSchemaEngine(nil))
	assert.Nil(t, h.Schema())
}

func TestRoleRegistrars_TheHolderReplacesAndClearsItsOwnRole(t *testing.T) {
	h := &engineHost{}
	grant := ordinaryGrant | core.CapPermissionChecker
	first := core.NewScopedHost(h, "permissions", grant)

	rules := &ruleChecker{rules: 1}
	require.NoError(t, first.RegisterPermissionChecker(rules))
	newer := &ruleChecker{rules: 2}
	require.NoError(t, first.RegisterPermissionChecker(newer), "the holder replaces its own checker")
	assert.Same(t, newer, h.PermissionChecker())

	// A hot reload starts the plugin again on a new scoped host under the
	// same name, and that instance stops in place of the first.
	restarted := core.NewScopedHost(h, "permissions", grant)
	require.NoError(t, restarted.RegisterPermissionChecker(nil))
	assert.Equal(t, core.PermissionChecker(core.RoleOnlyPermissionChecker{}), h.PermissionChecker())

	require.ErrorIs(t, restarted.RegisterPermissionChecker(nil), core.ErrHostRoleNotHeld,
		"a clear from a plugin that holds nothing is refused")
	require.NoError(t, restarted.RegisterPermissionChecker(rules), "the role is free again once its holder cleared it")
	assert.Same(t, rules, h.PermissionChecker())
}

// A clear from a plugin whose own registration never happened must not take
// away what the engine installed itself, which for storage is the local
// backend set at boot.
func TestRoleRegistrars_AClearFromAPluginThatNeverRegisteredLeavesTheEnginesStorage(t *testing.T) {
	h := &engineHost{}
	local := &roleStorage{}
	h.WithStorage(local)
	storage := core.NewScopedHost(h, "storage", ordinaryGrant|core.CapStorage)

	require.ErrorIs(t, storage.SetStorage(nil), core.ErrHostRoleNotHeld)
	assert.Same(t, local, h.Storage())
}

// Every role answers to its own capability and to no other, and once a
// plugin holds it, a second plugin with the same grant can neither replace it
// nor clear it.
func TestRoleRegistrars_EveryRoleAnswersToItsOwnGrant(t *testing.T) {
	type roleCase struct {
		role     core.HostRole
		need     core.Capability
		register func(sh *core.ScopedHost, clear bool) error
		installs func(h *engineHost) bool
	}
	var (
		checker     = &ruleChecker{}
		engine      = schemaengine.New(nil)
		sender      = &roleEmailSender{}
		validator   = &roleFlowValidator{}
		invoker     = &roleFlowInvoker{}
		localizer   = &roleLocalizer{}
		writer      = &roleEntryWriter{}
		revisions   = &roleRevisionStore{}
		storage     = &roleStorage{}
		provisioner = &roleUserProvisioner{}
	)
	cases := []roleCase{
		{
			role: core.HostRolePermissionChecker, need: core.CapPermissionChecker,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.RegisterPermissionChecker(nil)
				}
				return sh.RegisterPermissionChecker(checker)
			},
			installs: func(h *engineHost) bool { return h.PermissionChecker() == core.PermissionChecker(checker) },
		},
		{
			role: core.HostRoleSchemaEngine, need: core.CapSchema,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.RegisterSchemaEngine(nil)
				}
				return sh.RegisterSchemaEngine(engine)
			},
			installs: func(h *engineHost) bool { return h.Schema() == core.SchemaEngine(engine) },
		},
		{
			role: core.HostRoleEmailSender, need: core.CapEmailSender,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.RegisterEmailSender(nil)
				}
				return sh.RegisterEmailSender(sender)
			},
			installs: func(h *engineHost) bool { return h.EmailSender() == core.EmailSender(sender) },
		},
		{
			role: core.HostRoleFlowDefinitionValidator, need: core.CapFlowRegistry,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.RegisterFlowDefinitionValidator(nil)
				}
				return sh.RegisterFlowDefinitionValidator(validator)
			},
			installs: func(h *engineHost) bool {
				return h.FlowDefinitionValidator() == core.FlowDefinitionValidator(validator)
			},
		},
		{
			role: core.HostRoleFlowInvoker, need: core.CapFlowRegistry,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.RegisterFlowInvoker(nil)
				}
				return sh.RegisterFlowInvoker(invoker)
			},
			installs: func(h *engineHost) bool { return h.FlowInvoker() == core.FlowInvoker(invoker) },
		},
		{
			role: core.HostRoleContentLocalizer, need: core.CapContentLocalizer,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.RegisterContentLocalizer(nil)
				}
				return sh.RegisterContentLocalizer(localizer)
			},
			installs: func(h *engineHost) bool { return h.ContentLocalizer() == core.ContentLocalizer(localizer) },
		},
		{
			role: core.HostRoleContentEntryWriter, need: core.CapContentEntryWriter,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.RegisterContentEntryWriter(nil)
				}
				return sh.RegisterContentEntryWriter(writer)
			},
			installs: func(h *engineHost) bool { return h.ContentEntryWriter() == core.ContentEntryWriter(writer) },
		},
		{
			role: core.HostRoleRecordRevisionStore, need: core.CapRecordRevisionStore,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.RegisterRecordRevisionStore(nil)
				}
				return sh.RegisterRecordRevisionStore(revisions)
			},
			installs: func(h *engineHost) bool {
				return h.RecordRevisionStore() == core.RecordRevisionStore(revisions)
			},
		},
		{
			role: core.HostRoleStorage, need: core.CapStorage,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.SetStorage(nil)
				}
				return sh.SetStorage(storage)
			},
			installs: func(h *engineHost) bool { return h.Storage() == core.Storage(storage) },
		},
		{
			role: core.HostRoleUserProvisioner, need: core.CapUserProvisioner,
			register: func(sh *core.ScopedHost, clear bool) error {
				if clear {
					return sh.SetUserProvisioner(nil)
				}
				return sh.SetUserProvisioner(provisioner)
			},
			installs: func(h *engineHost) bool { return h.UserProvisioner() == core.UserProvisioner(provisioner) },
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			h := &engineHost{}
			// Every grant there is but this role's, the other roles included.
			without := core.NewScopedHost(h, "intruder", core.CapAll&^tc.need)
			holder := core.NewScopedHost(h, "holder", tc.need)
			peer := core.NewScopedHost(h, "peer", tc.need)

			require.ErrorIs(t, tc.register(without, false), core.ErrCapDenied)
			require.False(t, tc.installs(h), "a plugin without the grant installed the %s", tc.role)

			require.NoError(t, tc.register(holder, false))
			require.True(t, tc.installs(h), "the plugin holding the grant did not install the %s", tc.role)

			require.ErrorIs(t, tc.register(without, true), core.ErrCapDenied)
			require.ErrorIs(t, tc.register(peer, false), core.ErrHostRoleHeld)
			require.ErrorIs(t, tc.register(peer, true), core.ErrHostRoleHeld)
			require.True(t, tc.installs(h), "another plugin took the %s from its holder", tc.role)

			require.NoError(t, tc.register(holder, true))
			assert.False(t, tc.installs(h), "the holder's clear left the %s installed", tc.role)
		})
	}
}
