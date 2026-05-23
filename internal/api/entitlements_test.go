package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

func TestEntitlementsHandler_Unlicensed(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/entitlements", nil)

	entitlementsHandler(unlicensedEntitlements{}, false)(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	var snap licensing.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Plan != "free" {
		t.Errorf("plan = %q, want free", snap.Plan)
	}
	if len(snap.Features) != 0 {
		t.Errorf("features = %v, want empty with no license", snap.Features)
	}
}

func TestEntitlementsHandler_LicenseReportsFeatures(t *testing.T) {
	t.Parallel()

	ent := stubEntitlements{
		features: map[string]bool{"feature-a": true},
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/entitlements", nil)
	entitlementsHandler(ent, false)(rr, req)

	var snap licensing.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Features) != 1 || snap.Features[0] != "feature-a" {
		t.Errorf("features = %v, want [feature-a]", snap.Features)
	}
}

// The document names every value license_source can take, so a client typed
// from it does not refuse the answer for a key a super admin entered.
func TestEntitlementsSpec_NamesEveryLicenseSource(t *testing.T) {
	t.Parallel()

	op := buildOpenAPIDoc(nil, nil, openAPIOptions{}).Paths["/api/admin/entitlements"]["get"]
	if op == nil {
		t.Fatal("GET /api/admin/entitlements is not in the document")
	}
	for _, src := range []string{"token", "key", "stored_key"} {
		if !strings.Contains(op.Description, src) {
			t.Errorf("entitlements description does not name license_source %q", src)
		}
	}
}

// snapshotEntitlements answers one fixed snapshot and refuses no tenant
// anything.
type snapshotEntitlements struct{ snap licensing.Snapshot }

func (s snapshotEntitlements) Snapshot() licensing.Snapshot      { return s.snap }
func (snapshotEntitlements) Withholds(string, string) bool       { return false }
func (snapshotEntitlements) WithheldFrom(string) []string        { return []string{} }
func (snapshotEntitlements) Plugin(string) licensing.PluginGrant { return licensing.PluginGrant{} }

func TestEntitlementsHandler_CarriesThePlanLabelAndTheGraceEnd(t *testing.T) {
	expires := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	graceEnds := expires.Add(7 * 24 * time.Hour)

	inGrace := entitlementsJSON(t, snapshotEntitlements{snap: licensing.Snapshot{
		Plan: "example", PlanLabel: "Example", State: "grace", ExpiresAt: &expires, GraceEndsAt: &graceEnds,
	}}, true)
	assert.JSONEq(t, `"Example"`, string(inGrace["plan_label"]))
	assert.JSONEq(t, `"2026-10-01T00:00:00Z"`, string(inGrace["expires_at"]))
	assert.JSONEq(t, `"2026-10-08T00:00:00Z"`, string(inGrace["grace_ends_at"]))

	// An implementation that sets neither leaves both keys out, so an Open
	// build's body carries the base keys and the license_module flag.
	open, err := licensing.Open().NewManager(context.Background(), licensing.Env{})
	require.NoError(t, err)
	body := entitlementsJSON(t, open, false)
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"plan", "state", "features", "tenant_quota", "withheld", "caps", "license_module"}, keys)
}
