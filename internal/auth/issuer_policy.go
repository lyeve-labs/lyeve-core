package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// IssuerPolicy decides what a token from one trusted issuer may do here. The
// issuer signs who the caller is. The policy, set by the operator, decides the
// tenant they act in and the roles they hold, so nothing a token claims can
// make its holder super_admin or move them into another tenant.
type IssuerPolicy struct {
	// Issuer is the iss the token must carry, as listed in TRUSTED_ISSUERS.
	Issuer string `json:"issuer"`
	// Tenant is the one tenant the issuer's users act in. A token naming
	// another tenant is refused.
	Tenant string `json:"tenant"`
	// RolesClaim names the token claim holding the issuer's roles or groups.
	// Default "roles".
	RolesClaim string `json:"roles_claim,omitempty"`
	// RoleMap maps an issuer role to an engine role. A role with no entry is
	// dropped. super_admin cannot be a target.
	RoleMap map[string]string `json:"role_map,omitempty"`
	// DefaultRoles are held by every user of the issuer, whatever the token
	// says. super_admin cannot be one.
	DefaultRoles []string `json:"default_roles,omitempty"`
}

// ErrIssuerPolicy marks a token the policy refuses.
var ErrIssuerPolicy = errors.New("token refused by the issuer policy")

// Validate checks the policy before the engine boots on it.
func (p IssuerPolicy) Validate() error {
	if strings.TrimSpace(p.Issuer) == "" {
		return errors.New("issuer is required")
	}
	if !core.IsValidTenantSlug(p.Tenant) {
		return fmt.Errorf("issuer %s: tenant %q is not a tenant slug", p.Issuer, p.Tenant)
	}
	for from, to := range p.RoleMap {
		if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
			return fmt.Errorf("issuer %s: role_map has an empty role", p.Issuer)
		}
		if to == "super_admin" {
			return fmt.Errorf("issuer %s: role_map cannot grant super_admin", p.Issuer)
		}
	}
	for _, r := range p.DefaultRoles {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("issuer %s: default_roles has an empty role", p.Issuer)
		}
		if r == "super_admin" {
			return fmt.Errorf("issuer %s: default_roles cannot grant super_admin", p.Issuer)
		}
	}
	return nil
}

// Admit applies the policy to a token ParseExternal has already verified. It
// returns the engine roles the caller holds and the tenant they act in.
func (p IssuerPolicy) Admit(tokenStr string, claims *Claims) (roles []string, tenant string, err error) {
	if claims.TenantID != "" && claims.TenantID != p.Tenant {
		return nil, "", fmt.Errorf("%w: token names tenant %q, the issuer is pinned to %q", ErrIssuerPolicy, claims.TenantID, p.Tenant)
	}
	if strings.TrimSpace(claims.UserID) == "" {
		return nil, "", fmt.Errorf("%w: token has no subject", ErrIssuerPolicy)
	}
	if err := refuseCaseVariantClaims(tokenStr); err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrIssuerPolicy, err)
	}
	issuerRoles, err := claimStrings(tokenStr, p.rolesClaim())
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrIssuerPolicy, err)
	}
	set := map[string]bool{}
	for _, r := range p.DefaultRoles {
		set[r] = true
	}
	for _, r := range issuerRoles {
		if to, ok := p.RoleMap[r]; ok && to != "super_admin" {
			set[to] = true
		}
	}
	roles = make([]string, 0, len(set))
	for r := range set {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	return roles, p.Tenant, nil
}

func (p IssuerPolicy) rolesClaim() string {
	if p.RolesClaim == "" {
		return "roles"
	}
	return p.RolesClaim
}

// externalUserNamespace scopes the account ids derived for issuers' users.
var externalUserNamespace = uuid.MustParse("5b6a7f0e-2f4c-4d8a-9a3e-6c1d2b7e4f90")

// ExternalUserID is the local account id for subject at issuer: the same on
// every replica and every login, and never an id another issuer's user or a
// local account can hold.
func ExternalUserID(issuer, subject string) uuid.UUID {
	return uuid.NewSHA1(externalUserNamespace, []byte(issuer+"\n"+subject))
}

// payloadOf decodes the payload of an already verified token.
func payloadOf(tokenStr string) (map[string]any, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, errors.New("token is not a JWS")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	return payload, nil
}

// identityClaims are the claims the admission reads through a struct, whose
// decoder matches keys case insensitively and keeps the last: {"sub":"a",
// "Sub":"b"} names subject b. A payload that spells one of them twice is
// refused rather than read either way.
var identityClaims = []string{"sub", "email", "tenant_id"}

func refuseCaseVariantClaims(tokenStr string) error {
	payload, err := payloadOf(tokenStr)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for k := range payload {
		counts[strings.ToLower(k)]++
	}
	for _, name := range identityClaims {
		if counts[name] > 1 {
			return fmt.Errorf("claim %q appears under more than one spelling", name)
		}
	}
	return nil
}

// EmailVerified reports whether an already verified token asserts
// email_verified as true. Absent or anything else is false.
func EmailVerified(tokenStr string) bool {
	payload, err := payloadOf(tokenStr)
	if err != nil {
		return false
	}
	v, ok := payload["email_verified"].(bool)
	return ok && v
}

// claimStrings reads one claim of an already verified token as strings: a
// string, or an array whose string members are kept. An absent claim is none.
func claimStrings(tokenStr, name string) ([]string, error) {
	payload, err := payloadOf(tokenStr)
	if err != nil {
		return nil, err
	}
	switch v := payload[name].(type) {
	case nil:
		return nil, nil
	case string:
		return []string{v}, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("claim %q is neither a string nor a list", name)
	}
}
