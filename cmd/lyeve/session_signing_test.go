package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest/mockhost"
)

// signingHost signs every session it is asked for, the way the engine signs
// one for an active account.
type signingHost struct{ core.Host }

func (signingHost) SignSessionToken(context.Context, uuid.UUID, string, []string) (string, error) {
	return "session-token", nil
}

// A plugin that signs a user in is started with the grant to sign the session,
// whatever its own registration declares, so the scoped host it is started
// with signs. Every other plugin of this build is refused.
func TestCapPolicy_OnlySignInPluginsSignSessions(t *testing.T) {
	plugin.SetCapPolicy(capPolicy)
	t.Cleanup(func() { plugin.SetCapPolicy(nil) })

	signIn := map[string]bool{"magic-link": true, "mfa": true, "oauth": true, "saml": true}
	registered := core.RegisteredPlugins()
	for name := range signIn {
		if !slices.Contains(registered, name) {
			t.Errorf("%s signs a user in but this build does not register it", name)
		}
	}
	inner := signingHost{mockhost.New(t).Host()}
	for _, name := range registered {
		sh := core.NewScopedHost(inner, name, plugin.CapPolicy(name))
		tok, err := sh.SignSessionToken(context.Background(), uuid.New(), "user@example.com", nil)
		switch {
		case signIn[name] && (err != nil || tok != "session-token"):
			t.Errorf("%s signs a user in, but its scoped host returned %q, %v", name, tok, err)
		case !signIn[name] && !errors.Is(err, core.ErrCapDenied):
			t.Errorf("%s does not sign a user in, but its scoped host returned %q, %v", name, tok, err)
		}
	}
}

// keyHost serves the session signing secret to whoever asks, the way the
// engine's host does behind the scoped host.
type keyHost struct{ core.Host }

func (keyHost) Secret(key string) (string, bool) {
	if key == "jwt_secret" {
		return "signing-secret", true
	}
	return "", false
}

func (keyHost) Secrets(key string) ([]string, bool) {
	if key == "jwt_secrets" {
		return []string{"signing-secret"}, true
	}
	return nil, false
}

// The session signing secret reaches only the plugins that verify a session
// with it or open a value sealed under it. Holding CapConfigSecret is not
// enough, because the secret signs a session for any account on an install
// that signs with HMAC.
func TestCapPolicy_OnlySessionKeyReadersGetTheSigningSecret(t *testing.T) {
	plugin.SetCapPolicy(capPolicy)
	t.Cleanup(func() { plugin.SetCapPolicy(nil) })

	readers := map[string]bool{
		"ai": true, "captcha": true, "email": true, "grpc": true, "magic-link": true, "mfa": true,
		"multitenant": true, "oauth": true, "saml": true, "storage": true, "telemetry": true, "webhook": true,
	}
	inner := keyHost{mockhost.New(t).Host()}
	for _, name := range core.RegisteredPlugins() {
		sh := core.NewScopedHost(inner, name, plugin.CapPolicy(name))
		_, one := sh.Secret("jwt_secret")
		_, list := sh.Secrets("jwt_secrets")
		if one != readers[name] || list != readers[name] {
			t.Errorf("%s: jwt_secret served = %v, jwt_secrets served = %v, want %v", name, one, list, readers[name])
		}
	}
}

// stepUpHost accepts every step-up answer, the way the engine accepts a
// right one.
type stepUpHost struct{ core.Host }

func (stepUpHost) CheckStepUp(*http.Request, string, string) error { return nil }

// A step-up check spends the lockouts of the account the request's claims
// name, so only the plugins that ask a person to confirm an action on their
// own account reach it: analytics before an export, mfa before a first
// second factor. Every other plugin of this build is refused.
func TestCapPolicy_AnalyticsAndMFACheckStepUp(t *testing.T) {
	plugin.SetCapPolicy(capPolicy)
	t.Cleanup(func() { plugin.SetCapPolicy(nil) })

	askers := map[string]bool{"analytics": true, "mfa": true}
	inner := stepUpHost{mockhost.New(t).Host()}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/plugin-route", nil)
	for _, name := range core.RegisteredPlugins() {
		err := core.NewScopedHost(inner, name, plugin.CapPolicy(name)).CheckStepUp(r, core.StepUpPassword, "secret")
		switch {
		case askers[name] && err != nil:
			t.Errorf("%s asks for a step-up, but its scoped host returned %v", name, err)
		case !askers[name] && !errors.Is(err, core.ErrCapDenied):
			t.Errorf("%s does not ask for a step-up, but its scoped host returned %v", name, err)
		}
	}
}

// revokeHost removes one passkey for any account, the way the engine host
// does through the plugin that stores them.
type revokeHost struct{ core.Host }

func (revokeHost) RevokePasskeys(context.Context, uuid.UUID) (int, error) { return 1, nil }

// Removing an account's passkeys locks its owner out of a passkey sign-in,
// so only the plugin that resets a password reaches it. Every other plugin
// of this build is refused.
func TestCapPolicy_OnlyPasswordResetRevokesPasskeys(t *testing.T) {
	plugin.SetCapPolicy(capPolicy)
	t.Cleanup(func() { plugin.SetCapPolicy(nil) })

	inner := revokeHost{mockhost.New(t).Host()}
	for _, name := range core.RegisteredPlugins() {
		n, err := core.NewScopedHost(inner, name, plugin.CapPolicy(name)).RevokePasskeys(context.Background(), uuid.New())
		switch {
		case name == "password-reset" && (err != nil || n != 1):
			t.Errorf("password-reset removes passkeys at a reset, but its scoped host returned %d, %v", n, err)
		case name != "password-reset" && !errors.Is(err, core.ErrCapDenied):
			t.Errorf("%s does not remove passkeys, but its scoped host returned %d, %v", name, n, err)
		}
	}
}

// A password reset removes passkeys through the one plugin that stores them.
// The engine asks the role's running holder, and with no holder a reset
// removes nothing while the passkeys keep signing in, so this build has to
// link mfa as the role's only implementer.
func TestWiredRoles_MFAHoldsThePasskeyRevoker(t *testing.T) {
	role := reflect.TypeFor[core.PasskeyRevokerProvider]()
	wired := slices.ContainsFunc(plugin.WiredRoles(), func(r plugin.Role) bool {
		return r.Type == role && r.Single
	})
	if !wired {
		t.Fatalf("the engine does not wire %s as a single role", role)
	}
	var holders []string
	for _, name := range core.RegisteredPlugins() {
		if _, ok := plugin.LookupPlugin(name)().(core.PasskeyRevokerProvider); ok {
			holders = append(holders, name)
		}
	}
	slices.Sort(holders)
	if !slices.Equal(holders, []string{"mfa"}) {
		t.Errorf("%s is implemented by %v in this build, want mfa alone", role, holders)
	}
}
