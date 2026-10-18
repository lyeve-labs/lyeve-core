package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// passkeyOnlyStore reports every account enrolled through a passkey alone:
// enrolled, with no authenticator-app secret, which the MFA plugin answers
// as not found.
type passkeyOnlyStore struct {
	fakeMFAStore
}

func (s *passkeyOnlyStore) IsEnabled(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (s *passkeyOnlyStore) GetEnabled(context.Context, uuid.UUID) (string, []string, error) {
	return "", nil, fmt.Errorf("mfa: get enabled: %w", core.ErrNotFound)
}

// An account whose only factor is a passkey holds no code to send, so step-up
// does not ask it for one, and the refusal says why instead of reading as an
// outage. A password does not stand in for the passkey.
func TestStepUp_PasskeyOnlyAccountIsNotAskedForACode(t *testing.T) {
	rg := newTokenRig(t, WithMFAStore(&passkeyOnlyStore{}))
	_, session := rg.user("admin")
	issue := func(extra map[string]any) (int, string) {
		b := map[string]any{
			"name":       "ci",
			"grants":     []string{core.AdminGrantSchemasRead},
			"expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}
		for k, v := range extra {
			b[k] = v
		}
		rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session, body: b})
		return rec.Code, rec.Body.String()
	}

	for name, extra := range map[string]map[string]any{
		"nothing":  {},
		"a code":   {"mfa_code": "123456"},
		"password": {"password": tokenTestPassword},
	} {
		code, body := issue(extra)
		assert.Equal(t, http.StatusForbidden, code, "issuing with %s: %s", name, body)
		assert.Contains(t, body, "only second factor is a passkey", "issuing with %s", name)
		assert.NotContains(t, body, "mfa_code is required", "issuing with %s", name)
	}

	started := rg.startDevice("203.0.113.41:5000")
	rec := rg.approveWith(started.UserCode, session, map[string]any{"mfa_code": "123456"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "only second factor is a passkey")
}
