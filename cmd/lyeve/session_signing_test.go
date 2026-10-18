package main

import (
	"context"
	"errors"
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
