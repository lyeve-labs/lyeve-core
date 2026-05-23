package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The condition is invisible at runtime: the account signs in, resolves its
// scope, and every tenant-scoped read matches nothing rather than failing.
// The log line is the whole remedy, so the log line is what this asserts. It
// names the tenant and the account's digest, never the address, because log
// lines are stored.
//
// The roster belongs to whatever supplies it, so an install without one wires
// no validator and there is no roster to be missing from.
func TestWarnIfTenantMissing(t *testing.T) {
	present := func(context.Context, string) bool { return true }
	absent := func(context.Context, string) bool { return false }

	cases := map[string]struct {
		multiTenant bool
		tenant      string
		validator   core.TenantValidatorFunc
		warn        bool
	}{
		"multi-tenant, tenant absent from the roster": {true, "ghost", absent, true},
		"multi-tenant, tenant on the roster":          {true, "acme", present, false},
		"multi-tenant, no tenant on the account":      {true, "", absent, false},
		"single-tenant install is not checked":        {false, "ghost", absent, false},
		"no roster to be missing from":                {true, "ghost", nil, false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			h := &AuthHandler{multiTenant: c.multiTenant, tenantValidator: c.validator}
			h.warnIfTenantMissing(context.Background(), c.tenant, "person@example.com")

			got := strings.Contains(buf.String(), "not on the roster")
			if got != c.warn {
				t.Errorf("warned = %v, want %v; log was %q", got, c.warn, buf.String())
			}
			if strings.Contains(buf.String(), "person@example.com") {
				t.Errorf("the log names the address: %q", buf.String())
			}
			if c.warn && !strings.Contains(buf.String(), "subject_ref="+compliance.SubjectRef("person@example.com")) {
				t.Errorf("the log does not carry the digest: %q", buf.String())
			}
		})
	}
}

// Setup has to work on an install with no roster supplier, where there is no
// roster to register anything on.
func TestEnsureDefaultTenant(t *testing.T) {
	t.Run("no roster supplier means nothing to register", func(t *testing.T) {
		h := &AuthHandler{}
		if err := h.ensureDefaultTenant(context.Background()); err != nil {
			t.Errorf("setup must not fail without a roster: %v", err)
		}
	})

	t.Run("the roster supplier registers it", func(t *testing.T) {
		called := 0
		h := &AuthHandler{defaultTenant: func(context.Context) error { called++; return nil }}
		if err := h.ensureDefaultTenant(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called != 1 {
			t.Errorf("registered %d times, want 1", called)
		}
	})

	t.Run("a failure to register reaches the caller", func(t *testing.T) {
		boom := errors.New("roster is not writable")
		h := &AuthHandler{defaultTenant: func(context.Context) error { return boom }}
		err := h.ensureDefaultTenant(context.Background())
		if !errors.Is(err, boom) {
			t.Errorf("got %v, want it to wrap %v", err, boom)
		}
	})
}
