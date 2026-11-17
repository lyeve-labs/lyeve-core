package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

func listUsers(h *UsersHandler, query url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/admin/users?"+query.Encode(), nil)
	rr := httptest.NewRecorder()
	h.List(rr, r)
	return rr
}

// The refusals answer before the store is read, so the handler needs none.
func TestUsersList_RefusesAnOverlongQueryOrRole(t *testing.T) {
	h := NewUsersHandler(nil)
	cases := []struct {
		name  string
		query url.Values
		want  string
	}{
		{"q over 200 characters", url.Values{"q": {strings.Repeat("\u00e9", 201)}}, "The field q must not exceed 200 characters."},
		{"role over 200 characters", url.Values{"role": {strings.Repeat("r", 201)}}, "The field role must not exceed 200 characters."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := listUsers(h, tc.query)
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), tc.want)
		})
	}
}

func TestUsersList_FiltersByQueryAndRole(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	if testing.Short() {
		t.Skip("integration test")
	}
	users := db.NewUserStore(testdb.Postgres(t))
	ctx := context.Background()
	for email, roles := range map[string][]string{
		"ann@alpha.test":  {"editor"},
		"bob@alpha.test":  {"admin"},
		"carol@beta.test": {"admin"},
		"dana@beta.test":  {"marketing"},
	} {
		_, err := users.Create(ctx, email, "x", roles, "acme")
		require.NoError(t, err)
	}
	h := NewUsersHandler(users)

	emails := func(query url.Values) []string {
		rr := listUsers(h, query)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var out []struct {
			Email string `json:"email"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		got := []string{}
		for _, u := range out {
			got = append(got, u.Email)
		}
		return got
	}
	assert.ElementsMatch(t, []string{"ann@alpha.test", "bob@alpha.test"}, emails(url.Values{"q": {"  ALPHA  "}}))
	assert.ElementsMatch(t, []string{"bob@alpha.test"}, emails(url.Values{"q": {"alpha"}, "role": {"admin"}}))
	assert.ElementsMatch(t, []string{"dana@beta.test"}, emails(url.Values{"role": {" marketing "}}), "any role name is a filter, trimmed")
	assert.Empty(t, emails(url.Values{"role": {"sales"}}), "a role nobody holds lists nobody")
	assert.Len(t, emails(url.Values{"q": {"   "}, "role": {"  "}}), 4, "a blank q or role narrows nothing")
}
