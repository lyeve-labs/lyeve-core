package compliance

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The addresses an erasure reaches
//
// A plugin keeps a person's rows under an address: where it sent mail, or
// what a form posted. One account can hold several. It signs in with one, its
// identity provider may assert another, and every identity linked to it
// brings its own. The erasure resolves them all at the start of each run,
// from the rows as they stand then, and hands them to every eraser on the
// context. Nothing keeps them past the run: an address kept for a retry would
// reach whoever holds it by then.

// SubjectAddressFunc returns the addresses a plugin keeps for one account,
// such as the address of each identity linked to it. host is scoped as it is
// for SubjectEraseFunc.
type SubjectAddressFunc func(ctx context.Context, host core.Host, accountID string) ([]string, error)

// SubjectAddress is one address an erasure or export reaches, and the tenant it is reached in.
// Tenant is empty for the identifier the caller named, which keeps its reach.
type SubjectAddress struct{ Address, Tenant string }

// SubjectAccount is one account an identifier names: its id, its home tenant
// (core.DefaultTenantSlug for an account of the install) and the addresses
// its record holds.
type SubjectAccount struct {
	ID, Tenant string
	Addresses  []string
}

// SubjectAccountResolver is the engine's account eraser, which names the
// accounts an identifier resolves to. ResolveSubjectAddresses reads it from
// the erasers it is handed.
type SubjectAccountResolver interface {
	SubjectAccounts(ctx context.Context, identifier string) ([]SubjectAccount, error)
}

// SubjectAccounts returns the unerased accounts identifier names, each with
// the addresses its record holds in email and provider_email. An identifier
// names an account by its id, or by an address the account holds in either
// column, matched as SameAddress matches. A disabled account is found,
// because the erasure's own sign-out disables it.
func SubjectAccounts(ctx context.Context, host core.Host, identifier string) ([]SubjectAccount, error) {
	d := host.Dialect()
	notErased := "0"
	if d == "postgres" {
		notErased = "false"
	}
	where, args := "("+AddressCandidates("email", 1)+" OR "+AddressCandidates("provider_email", 1)+")", AddressForms(identifier)
	id, perr := uuid.Parse(identifier)
	byID := perr == nil
	if byID {
		where, args = "id = $1", []any{id}
	}
	rows, err := host.Querier(ctx).Query(ctx,
		// lyeve:cross-tenant sys_users an erasure follows one person across every tenant: the admin DSAR routes name the subject by account id or by address and require super_admin, and the self-service erasure names only the caller's own account, by id
		`SELECT id, email, provider_email, tenant_id FROM sys_users WHERE `+where+` AND anonymized = `+notErased, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve subject accounts: %w", err)
	}
	defer rows.Close()
	var accounts []SubjectAccount
	for rows.Next() {
		var account uuid.UUID
		var email, tenant string
		var provider sql.NullString
		if err := rows.Scan(core.ScanUUID(d, &account), &email, &provider, &tenant); err != nil {
			return nil, fmt.Errorf("resolve subject accounts: %w", err)
		}
		if !byID && !SameAddress(email, identifier) && !SameAddress(provider.String, identifier) {
			continue
		}
		if tenant == "" {
			tenant = core.DefaultTenantSlug
		}
		accounts = append(accounts, SubjectAccount{ID: account.String(), Tenant: tenant, Addresses: []string{email, provider.String}})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve subject accounts: %w", err)
	}
	return accounts, nil
}

type subjectAddressesKey struct{}

// ResolveSubjectAddresses returns ctx carrying the addresses of the subject
// identifier names, which SubjectAddresses reads. The identifier itself,
// unless it is an account id, keeps the reach the caller gave it. Every other
// address comes from an account a SubjectAccountResolver among erasers
// resolves it to: the account's own two, and those every declared
// SubjectErasure.Addresses holds for it. The account's tenant controls those,
// through its identity provider or its admins, so each is reached in that
// tenant only. Each run that reads them resolves them again, and an error
// leaves the set unknown, so the run must not go ahead.
func ResolveSubjectAddresses(ctx context.Context, identifier string, erasers []SubjectEraser) (context.Context, error) {
	var set []SubjectAddress
	add := func(tenant string, addresses []string) {
		for _, a := range addresses {
			if a == "" || slices.ContainsFunc(set, func(held SubjectAddress) bool {
				return (held.Tenant == "" || held.Tenant == tenant) && SameAddress(held.Address, a)
			}) {
				continue
			}
			set = append(set, SubjectAddress{Address: a, Tenant: tenant})
		}
	}
	if _, err := uuid.Parse(identifier); err != nil {
		add("", []string{identifier})
	}
	var accounts []SubjectAccount
	for _, e := range erasers {
		if r, ok := e.(SubjectAccountResolver); ok {
			found, err := r.SubjectAccounts(ctx, identifier)
			if err != nil {
				return ctx, err
			}
			accounts = append(accounts, found...)
		}
	}
	for _, a := range accounts {
		add(a.Tenant, a.Addresses)
	}
	for _, e := range erasers {
		d, ok := e.(*DeclaredEraser)
		if !ok {
			continue
		}
		for _, a := range accounts {
			addresses, err := d.accountAddresses(ctx, a.ID)
			if err != nil {
				return ctx, fmt.Errorf("%s subject addresses: %w", d.name, err)
			}
			add(a.Tenant, addresses)
		}
	}
	ctx = context.WithValue(ctx, requestTenantKey{}, core.TenantIDFromCtx(ctx))
	return context.WithValue(ctx, subjectAddressesKey{}, set), nil
}

type requestTenantKey struct{}

// RequestTenant returns the tenant the request that resolved the subject's
// addresses runs in. An address no account holds reaches that tenant only. A
// sweep binds each run to its own tenant, and an exporter that read the run's
// tenant there listed such an address in every tenant, while the erasure of
// the same request reached one. A context no run resolved answers with its
// own tenant.
func RequestTenant(ctx context.Context) string {
	if tenant, ok := ctx.Value(requestTenantKey{}).(string); ok {
		return tenant
	}
	return core.TenantIDFromCtx(ctx)
}

// SubjectAddresses returns the addresses of the subject identifier names, as
// ResolveSubjectAddresses put them on ctx, each with the tenant it is reached
// in. An eraser matches its address-keyed rows against each of them instead
// of looking the account up itself. A context no erasure run resolved, such
// as a test calling a store directly, carries none, and the identifier then
// stands for itself unless it is an account id.
func SubjectAddresses(ctx context.Context, identifier string) []SubjectAddress {
	if set, ok := ctx.Value(subjectAddressesKey{}).([]SubjectAddress); ok {
		return slices.Clone(set)
	}
	if _, err := uuid.Parse(identifier); err == nil {
		return nil
	}
	return []SubjectAddress{{Address: identifier}}
}

// canonicalSubject spells an account id as the account records hold it, so
// every eraser and the account eraser name the same account. An id in upper
// case, in braces, with a urn:uuid: prefix or without hyphens otherwise
// reached the plugins' rows through the parsed id while the account eraser,
// which compares the text, found no account and erased nothing. Any other
// identifier is returned as given.
func canonicalSubject(identifier string) string {
	if id, err := uuid.Parse(identifier); err == nil {
		return id.String()
	}
	return identifier
}
