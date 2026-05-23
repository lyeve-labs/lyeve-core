package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupToken_Matches(t *testing.T) {
	t.Parallel()
	env := NewSetupTokenFromEnv("operator-setup-token-0123")
	cases := []struct {
		name      string
		token     *SetupToken
		presented string
		want      bool
	}{
		{"configured token", env, "operator-setup-token-0123", true},
		{"wrong token", env, "operator-setup-token-0124", false},
		{"prefix of the token", env, "operator-setup", false},
		{"empty presented", env, "", false},
		{"nil token", nil, "operator-setup-token-0123", false},
		{"unconfigured token", NewSetupTokenFromEnv(""), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.token.Matches(tc.presented))
		})
	}
}

func TestSetupToken_GeneratedIsRandomAndNamesTheLog(t *testing.T) {
	t.Parallel()
	a, plainA, err := GenerateSetupToken()
	require.NoError(t, err)
	_, plainB, err := GenerateSetupToken()
	require.NoError(t, err)

	assert.NotEqual(t, plainA, plainB)
	assert.GreaterOrEqual(t, len(plainA), 32)
	assert.True(t, a.Matches(plainA))
	assert.False(t, a.Matches(plainB))
	assert.Equal(t, SetupTokenFromLog, a.Source())
}

func TestSetupToken_RetireEndsIt(t *testing.T) {
	t.Parallel()
	tok := NewSetupTokenFromEnv("operator-setup-token-0123")
	require.Equal(t, SetupTokenFromEnv, tok.Source())

	tok.Retire()

	assert.False(t, tok.Matches("operator-setup-token-0123"))
	assert.Empty(t, tok.Source())
}

func TestSetup_RefusesWithoutToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		wired  *SetupToken
		body   string
		header string
	}{
		{"no token wired", nil, `{"email":"a@b.test","password":"CorrectHorseBatteryStaple1!","setup_token":"anything-at-all-123"}`, ""},
		{"token absent", NewSetupTokenFromEnv("operator-setup-token-0123"), `{"email":"a@b.test","password":"CorrectHorseBatteryStaple1!"}`, ""},
		{"token wrong in body", NewSetupTokenFromEnv("operator-setup-token-0123"), `{"email":"a@b.test","password":"CorrectHorseBatteryStaple1!","setup_token":"guess"}`, ""},
		{"token wrong in header", NewSetupTokenFromEnv("operator-setup-token-0123"), `{"email":"a@b.test","password":"CorrectHorseBatteryStaple1!"}`, "guess"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A store that fails every call proves the refusal comes before
			// the database is consulted at all.
			h := NewAuthHandler(&dbErrorUserStore{}, nil, nil, "secret", 3600, false)
			if tc.wired != nil {
				h.WithSetupToken(tc.wired)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set(SetupTokenHeader, tc.header)
			}
			rr := httptest.NewRecorder()

			h.Setup(rr, req)

			assert.Equal(t, http.StatusUnauthorized, rr.Code)
			assert.Contains(t, rr.Body.String(), "AUTH_SETUP_TOKEN_INVALID")
		})
	}
}
