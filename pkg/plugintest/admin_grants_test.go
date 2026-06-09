package plugintest

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type recordingT struct {
	errs []string
}

func (r *recordingT) Helper() {}
func (r *recordingT) Errorf(format string, a ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, a...))
}
func (r *recordingT) Fatalf(format string, a ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, a...))
}
func (r *recordingT) Logf(string, ...any) {}

func TestRequireNoSessionOnlyGrants(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	rec := &recordingT{}
	RequireNoSessionOnlyGrants(rec, []core.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/webhooks", Group: core.GroupAdmin, Handler: ok, AdminGrant: core.AdminGrantWebhooksRead},
		{Method: http.MethodPost, Pattern: "/api/admin/webhooks", Group: core.GroupAdmin, Handler: ok},
	})
	assert.Empty(t, rec.errs, "a grant on an open route and a route with none both pass")

	rec = &recordingT{}
	RequireNoSessionOnlyGrants(rec, []core.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/users/{id}", Group: core.GroupSuperAdmin, Handler: ok, AdminGrant: core.AdminGrantContentRead},
		{Method: http.MethodGet, Pattern: "/api/admin/things", Group: core.GroupAdmin, Handler: ok, AdminGrant: "things:read"},
	})
	if assert.Len(t, rec.errs, 2) {
		assert.Contains(t, rec.errs[0], "session only")
		assert.Contains(t, rec.errs[1], "not in the catalog")
	}
}
