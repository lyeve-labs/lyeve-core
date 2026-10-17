package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkerHost holds a permission checker the way the engine host does, and
// keeps the record of holders a scoped host needs before it hands one over.
type checkerHost struct {
	*capturingHost
	HostRoleRegistry
	checker PermissionChecker
	calls   int
}

func (h *checkerHost) RegisterPermissionChecker(c PermissionChecker) error {
	h.calls++
	h.checker = c
	return nil
}

// recordlessHost takes a permission checker but keeps no record of who
// registered it.
type recordlessHost struct {
	*capturingHost
	calls int
}

func (h *recordlessHost) RegisterPermissionChecker(PermissionChecker) error {
	h.calls++
	return nil
}

type fixedChecker struct{ allow bool }

func (c *fixedChecker) Allowed(context.Context, []string, string, string) (bool, error) {
	return c.allow, nil
}

// roleRefusals returns every "plugin host role refused" record the host
// logged.
func roleRefusals(t *testing.T, h *capturingHost) []logRecord {
	t.Helper()
	var out []logRecord
	for _, line := range bytes.Split([]byte(h.buf.String()), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec logRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %s (%v)", line, err)
		}
		if rec["msg"] == "plugin host role refused" {
			out = append(out, rec)
		}
	}
	return out
}

// A plugin built before the registrars answered discards the error, so the
// log line has to say what was refused and why.
func TestScopedHostRoles_ARefusalForTheGrantIsLoggedAndReturned(t *testing.T) {
	h := &checkerHost{capturingHost: newCapturingHost()}
	sh := NewScopedHost(h, "ab-testing", CapDBWrite|CapRoutes)

	err := sh.RegisterPermissionChecker(&fixedChecker{allow: true})
	require.ErrorIs(t, err, ErrCapDenied)
	assert.Equal(t, 0, h.calls, "the host was handed a checker from a plugin without the grant")

	recs := denialRecords(t, h.capturingHost)
	require.Len(t, recs, 1, h.buf.String())
	assertField(t, recs[0], "plugin", "ab-testing")
	assertField(t, recs[0], "op", "RegisterPermissionChecker")
	assertField(t, recs[0], "capability", "CapPermissionChecker")
	assertField(t, recs[0], "target", "permission checker")
}

func TestScopedHostRoles_ARoleAnotherPluginHoldsIsRefusedAndLogged(t *testing.T) {
	h := &checkerHost{capturingHost: newCapturingHost()}
	holder := NewScopedHost(h, "permissions", CapPermissionChecker)
	peer := NewScopedHost(h, "rules-two", CapPermissionChecker)

	rules := &fixedChecker{}
	require.NoError(t, holder.RegisterPermissionChecker(rules))

	err := peer.RegisterPermissionChecker(&fixedChecker{allow: true})
	require.ErrorIs(t, err, ErrHostRoleHeld)
	assert.Same(t, rules, h.checker)
	err = peer.RegisterPermissionChecker(nil)
	require.ErrorIs(t, err, ErrHostRoleHeld)
	assert.Same(t, rules, h.checker)

	recs := roleRefusals(t, h.capturingHost)
	require.Len(t, recs, 1, "one line per distinct refusal: %s", h.buf.String())
	assertField(t, recs[0], "plugin", "rules-two")
	assertField(t, recs[0], "op", "RegisterPermissionChecker")
	assertField(t, recs[0], "role", "permission checker")
}

// A host that cannot record who holds a role could not refuse the next plugin
// that replaced it, so it is handed nothing.
func TestScopedHostRoles_AHostThatKeepsNoHolderRecordTakesNoRole(t *testing.T) {
	h := &recordlessHost{capturingHost: newCapturingHost()}
	sh := NewScopedHost(h, "permissions", CapPermissionChecker)

	err := sh.RegisterPermissionChecker(&fixedChecker{})
	require.ErrorIs(t, err, ErrHostRoleUnsupported)
	assert.Equal(t, 0, h.calls)
	require.Len(t, roleRefusals(t, h.capturingHost), 1, h.buf.String())
}

// A host without the role's registrar refuses, where it used to swallow the
// registration, so the plugin learns that nothing serves its role. The error
// tells that apart from a refusal, so a plugin can carry on there.
func TestScopedHostRoles_AHostWithoutTheRegistrarRefuses(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "email", CapEmailSender)

	err := sh.RegisterEmailSender(&stubEmailSender{})
	require.ErrorIs(t, err, ErrHostRoleUnsupported)
	assert.ErrorContains(t, err, "takes no email sender")
	assert.NotErrorIs(t, err, ErrCapDenied)
	recs := roleRefusals(t, h)
	require.Len(t, recs, 1, h.buf.String())
	assertField(t, recs[0], "role", "email sender")
}

// The scoped host must not expose the holder record itself, or a plugin could
// claim a role under another plugin's name.
func TestScopedHostRoles_APluginCannotReachTheHolderRecord(t *testing.T) {
	var host Host = NewScopedHost(&checkerHost{capturingHost: newCapturingHost()}, "ab-testing", CapAll)
	_, ok := host.(HostRoleOwnerRegistrar)
	assert.False(t, ok)
}

func TestHostRoleRegistry_AFailedSetLeavesTheRecord(t *testing.T) {
	var r HostRoleRegistry
	failed := errors.New("set failed")

	require.ErrorIs(t, r.RegisterOwnedRole("a", HostRoleStorage, false, func() error { return failed }), failed)
	require.NoError(t, r.RegisterOwnedRole("b", HostRoleStorage, false, func() error { return nil }),
		"a registration that failed left a holder behind")

	require.ErrorIs(t, r.RegisterOwnedRole("b", HostRoleStorage, true, func() error { return failed }), failed)
	require.ErrorIs(t, r.RegisterOwnedRole("a", HostRoleStorage, false, func() error { return nil }), ErrHostRoleHeld,
		"a clear that failed released the role")
}

func TestHostRoleRegistry_RolesAreHeldApart(t *testing.T) {
	var r HostRoleRegistry
	noop := func() error { return nil }
	require.NoError(t, r.RegisterOwnedRole("flow", HostRoleFlowInvoker, false, noop))
	require.NoError(t, r.RegisterOwnedRole("permissions", HostRolePermissionChecker, false, noop))
	require.ErrorIs(t, r.RegisterOwnedRole("flow", HostRolePermissionChecker, true, noop), ErrHostRoleHeld)
	require.NoError(t, r.RegisterOwnedRole("flow", HostRoleFlowInvoker, true, noop))
}

// Plugins start in parallel. The record and the value it guards change
// together, so however the registrations interleave, exactly one plugin ends
// up holding the role and the installed value is that plugin's.
func TestHostRoleRegistry_ConcurrentRegistrationsLeaveOneHolder(t *testing.T) {
	var (
		r         HostRoleRegistry
		installed string
		wg        sync.WaitGroup
		mu        sync.Mutex
		won       []string
	)
	for i := range 16 {
		owner := fmt.Sprintf("plugin-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.RegisterOwnedRole(owner, HostRoleSchemaEngine, false, func() error {
				installed = owner
				return nil
			})
			if err == nil {
				mu.Lock()
				won = append(won, owner)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Len(t, won, 1)
	assert.Equal(t, won[0], installed)
}
