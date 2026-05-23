package runtime

import (
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// issuersWithoutPolicy lists the TRUSTED_ISSUERS entries no policy covers.
// Their tokens are never tried, so an operator who listed an issuer and
// forgot its policy would otherwise see every login from it refused with
// nothing in the log to say why.
func issuersWithoutPolicy(cfg *config.Config) []string {
	covered := map[string]bool{}
	for _, p := range cfg.TrustedIssuerPolicies {
		covered[p.Issuer] = true
	}
	var out []string
	for _, iss := range cfg.TrustedIssuers {
		if !covered[iss] {
			out = append(out, iss)
		}
	}
	return out
}

// issuersPinnedOffRoster lists the issuers whose policy pins a tenant this
// install does not serve. Their users sign in, act in that tenant and read
// nothing, since every tenant-scoped query matches no rows, so the policy
// looks broken from the issuer's side with nothing to say why.
//
// A single-tenant install serves core.DefaultTenantSlug alone and keeps no
// roster to read. A multi-tenant one serves what onRoster finds.
//
// It reports rather than refuses: a tenant can be registered after boot, and
// a policy whose tenant was deleted must not stop the engine restarting.
func issuersPinnedOffRoster(cfg *config.Config, onRoster func(slug string) (bool, error)) ([]string, error) {
	var out []string
	for _, p := range cfg.TrustedIssuerPolicies {
		if !cfg.MultiTenant {
			if p.Tenant != core.DefaultTenantSlug {
				out = append(out, p.Issuer)
			}
			continue
		}
		ok, err := onRoster(p.Tenant)
		if err != nil {
			return out, err
		}
		if !ok {
			out = append(out, p.Issuer)
		}
	}
	return out, nil
}
