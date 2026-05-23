// Package tests: plugin activation integration coverage.
//
// Exercises the full HTTP-stack activation path. Requires TEST_DATABASE_URL.
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// TestActivation_StatusEndpoint walks the plugin status report through a real
// admin router. No plugin module is blank-imported here:
// verify-no-plugin-source.sh refuses plugin imports in this module.
func TestActivation_StatusEndpoint(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()

	// Reset users so the setup endpoint creates a fresh super_admin.
	if _, err := pool.Exec(context.Background(), "DELETE FROM sys_users"); err != nil {
		t.Fatalf("clear sys_users: %v", err)
	}

	t.Run("open_entitles_what_the_build_compiled", func(t *testing.T) {
		open, err := licensing.Open().NewManager(context.Background(), licensing.Env{Names: core.CompiledFeatureNames()})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		act := plugin.NewActivator(nil /* host unused, since the activator never calls Start here */, nil)
		act.Resolve(open, "")

		router, _ := api.NewAdminRouter(pool, cfg, api.WithPluginStatus(act))
		token := bootstrapSuperAdmin(t, router, "unlicensed@test.local", "password123")

		rr := doJSON(t, router, "GET", "/api/admin/plugins/status", "",
			map[string]string{"Authorization": "Bearer " + token})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body)
		}
		if got := rr.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type: got %q, want application/json", got)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control: got %q, want no-store", got)
		}

		var report plugin.PluginStatusReport
		if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode body: %v (raw=%q)", err, rr.Body.String())
		}
		// Nothing is blank-imported in this binary, so a build that links no
		// licensing implementation entitles nothing, and says so with an
		// empty list rather than null.
		if report.Entitled == nil || len(report.Entitled) != 0 {
			t.Errorf("open with no plugin compiled: entitled = %#v, want an empty list", report.Entitled)
		}
	})

	t.Run("the_report_lists_what_the_policy_entitles", func(t *testing.T) {
		// Reset users for a clean super_admin setup.
		if _, err := pool.Exec(context.Background(), "DELETE FROM sys_users"); err != nil {
			t.Fatalf("clear sys_users: %v", err)
		}
		act := plugin.NewActivator(nil, nil)
		act.Resolve(entitledPolicy{"plugin-a", "plugin-b"}, "")

		router, _ := api.NewAdminRouter(pool, cfg, api.WithPluginStatus(act))
		token := bootstrapSuperAdmin(t, router, "entitled@test.local", "password123")

		rr := doJSON(t, router, "GET", "/api/admin/plugins/status", "",
			map[string]string{"Authorization": "Bearer " + token})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body)
		}

		var report plugin.PluginStatusReport
		if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode body: %v (raw=%q)", err, rr.Body.String())
		}
		// A policy that names what it entitles has the report list exactly
		// that, plugins compiled in or not.
		if !slices.Equal(report.Entitled, []string{"plugin-a", "plugin-b"}) {
			t.Errorf("entitled = %v, want [plugin-a plugin-b]", report.Entitled)
		}
	})

	t.Run("requires_auth", func(t *testing.T) {
		act := plugin.NewActivator(nil, nil)
		act.Resolve(entitledPolicy{}, "")
		router, _ := api.NewAdminRouter(pool, cfg, api.WithPluginStatus(act))

		rr := doJSON(t, router, "GET", "/api/admin/plugins/status", "", nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 without auth, got %d: %s", rr.Code, rr.Body)
		}
	})

	t.Run("nil_provider_yields_empty_report", func(t *testing.T) {
		// Reset users. This scenario verifies the graceful-degradation path
		// when the engine boots without an activator (e.g. development binary
		// built without WithPluginStatus).
		if _, err := pool.Exec(context.Background(), "DELETE FROM sys_users"); err != nil {
			t.Fatalf("clear sys_users: %v", err)
		}
		router, _ := api.NewAdminRouter(pool, cfg) // no WithPluginStatus
		token := bootstrapSuperAdmin(t, router, "nil@test.local", "password123")

		rr := doJSON(t, router, "GET", "/api/admin/plugins/status", "",
			map[string]string{"Authorization": "Bearer " + token})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body)
		}

		var report plugin.PluginStatusReport
		if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(report.Compiled) != 0 || len(report.Entitled) != 0 || len(report.Plugins) != 0 {
			t.Errorf("nil provider: expected empty report, got %+v", report)
		}
	})
}

// entitledPolicy entitles exactly the names it lists, and names every one of
// them to the status report.
type entitledPolicy []string

func (p entitledPolicy) Plugin(name string) licensing.PluginGrant {
	return licensing.PluginGrant{Start: slices.Contains(p, name)}
}

func (p entitledPolicy) EntitledNames() []string { return append([]string(nil), p...) }

// bootstrapSuperAdmin runs the first-time setup flow against the admin
// router and returns the JWT for the created super_admin. Tests that
// exercise auth-protected endpoints call this once per scenario.
func bootstrapSuperAdmin(t *testing.T, router http.Handler, email, password string) string {
	t.Helper()
	rr := doJSON(t, router, "POST", "/api/admin/setup",
		mustJSON(t, map[string]string{"email": email, "password": password}), setupHeaders)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rr.Code, rr.Body)
	}
	body := parseBody(t, rr)
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("setup returned empty token: %v", body)
	}
	return tok
}
