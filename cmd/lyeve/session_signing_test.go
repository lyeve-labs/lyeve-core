package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
// name, so only the plugin that asks a person to confirm their own export
// reaches it. Every other plugin of this build is refused.
func TestCapPolicy_OnlyAnalyticsChecksStepUp(t *testing.T) {
	plugin.SetCapPolicy(capPolicy)
	t.Cleanup(func() { plugin.SetCapPolicy(nil) })

	inner := stepUpHost{mockhost.New(t).Host()}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/plugin-route", nil)
	for _, name := range core.RegisteredPlugins() {
		err := core.NewScopedHost(inner, name, plugin.CapPolicy(name)).CheckStepUp(r, core.StepUpPassword, "secret")
		switch {
		case name == "analytics" && err != nil:
			t.Errorf("analytics asks for a step-up, but its scoped host returned %v", err)
		case name != "analytics" && !errors.Is(err, core.ErrCapDenied):
			t.Errorf("%s does not ask for a step-up, but its scoped host returned %v", name, err)
		}
	}
}
