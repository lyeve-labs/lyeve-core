package api

import (
	"net/http"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// withTenantGate refuses a plugin's route to a tenant the plugin is withheld
// from. The super admin group is not wrapped: those routes run the instance,
// such as creating tenants, and withholding a plugin from the tenant a super
// admin happens to be scoped to must not lock the operator out of it.
func withTenantGate(withholds func(tenant, name string) bool, pluginName string, next http.Handler) http.Handler {
	if withholds == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if withholds(core.TenantIDFromCtx(r.Context()), pluginName) {
			respondErrCode(w, r, http.StatusForbidden, i18n.CodeTenantFeatureWithheld, nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
