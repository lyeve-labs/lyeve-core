package db

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
)

// Account addresses are compared in the form core.NormalizeEmail gives them,
// and that form now includes Unicode normalization form C. A row written
// before holds the address as it was typed: lowered, but possibly with e and a
// combining accent where the form has a precomposed é. A sign-in then misses
// it on PostgreSQL, which compares bytes, and every dialect hands those bytes
// to the code that matches a person's rows by address. MySQL and SQL Server
// have no function for the normalization form, so the rewrite runs here, in
// Go, at boot.
//
// Two accounts can hold one address in two forms on PostgreSQL, where the
// unique keys compare bytes. The oldest account, the earliest created and
// then the lowest id, takes the address, unless one of them already holds the
// normalized form, which it keeps. The others keep their bytes and are
// reported on every boot until an operator decides between them. Nothing is
// merged or deleted. email is unique across the install, so its accounts
// collide whatever their tenant. provider_email is unique per tenant.

// accountEmailFormLock is the lock one replica holds while it rewrites. The
// others skip the step, and the rewrite would be safe without it, because
// each update names the bytes it replaces.
const accountEmailFormLock = "account_email_form"

// accountEmailFormBudget bounds the step, so a large table cannot hold the
// boot. A run cut short leaves every row it did not reach as it was, and the
// next boot continues from them.
const accountEmailFormBudget = 30 * time.Second

// accountEmailCollisionLines caps the lines one boot writes about collisions.
// The summary line still counts them all.
const accountEmailCollisionLines = 50

// AccountEmailCollision is a set of accounts whose addresses are one address
// once normalized. It names accounts and tenants by id, never the address,
// because the log it is written to is read by administrators and an address
// is personal data.
type AccountEmailCollision struct {
	Column string
	// HolderID is the account that holds the normalized address. uuid.Nil
	// when it was found only by the unique key refusing the rewrite.
	HolderID uuid.UUID
	// AccountIDs are the accounts that keep their stored address.
	AccountIDs []uuid.UUID
	// TenantIDs are the tenants of the holder and those accounts.
	TenantIDs []string
}

// AccountEmailFormReport is what one run of NormalizeAccountEmails did.
type AccountEmailFormReport struct {
	Rewritten  int
	Collisions []AccountEmailCollision
	// Skipped reports that another replica held the lock.
	Skipped bool
}

// accountAddressRow is one account's value in one address column.
type accountAddressRow struct {
	id      uuid.UUID
	tenant  string
	value   string
	created time.Time
}

// NormalizeAccountEmails rewrites every account address that is not in the
// form core.NormalizeEmail gives it, and logs the accounts whose addresses
// collide in that form. It touches only rows whose stored bytes differ from
// the form, so a second run rewrites nothing and reports the same collisions.
func NormalizeAccountEmails(ctx context.Context, pool DB, logger *slog.Logger) (AccountEmailFormReport, error) {
	var report AccountEmailFormReport
	ctx, cancel := context.WithTimeout(ctx, accountEmailFormBudget)
	defer cancel()

	if raw, ok := pool.(interface{ SQLDB() *sql.DB }); ok && raw.SQLDB() != nil {
		lock := engine.NewDistLock(accountEmailFormLock)
		held, err := lock.TryAcquire(ctx, raw.SQLDB(), pool.Engine())
		if err != nil {
			return report, fmt.Errorf("normalize account emails: lock: %w", err)
		}
		if !held {
			report.Skipped = true
			return report, nil
		}
		defer func() { _ = lock.Release() }()
	}
	// A run cut short by an error or the deadline still reports what it
	// rewrote and found.
	defer func() { logAccountEmailForm(logger, report) }()

	return report, rewriteEveryAccountAddress(ctx, pool, &report)
}

// rewriteEveryAccountAddress rewrites every candidate row in both address
// columns, adding what it did to report.
func rewriteEveryAccountAddress(ctx context.Context, pool DB, report *AccountEmailFormReport) error {
	byColumn, err := accountAddressCandidates(ctx, pool)
	if err != nil {
		return err
	}
	for _, column := range []string{"email", "provider_email"} {
		if err := rewriteAccountAddresses(ctx, pool, column, byColumn[column], report); err != nil {
			return err
		}
	}
	return nil
}

// accountAddressCandidates reads every account whose email or provider_email
// holds a character outside printable ASCII. Only those can change: the form
// leaves ASCII as it is, and ASCII rows were lowered when they were written or
// by the migration that folded case.
func accountAddressCandidates(ctx context.Context, pool DB) (map[string][]accountAddressRow, error) {
	engineName := pool.Engine()
	// lyeve:cross-tenant sys_users the boot rewrite reads every tenant's accounts, and email collides across tenants
	rows, err := pool.Query(ctx, `SELECT id, tenant_id, email, provider_email, created_at FROM sys_users WHERE `+
		outsideASCII(engineName, "email")+` OR `+outsideASCII(engineName, "provider_email"))
	if err != nil {
		return nil, fmt.Errorf("normalize account emails: select: %w", err)
	}
	defer rows.Close()
	out := map[string][]accountAddressRow{}
	for rows.Next() {
		var (
			r        accountAddressRow
			email    string
			provider sql.NullString
		)
		if err := rows.Scan(scanUUID(engineName, &r.id), &r.tenant, &email, &provider, &r.created); err != nil {
			return nil, fmt.Errorf("normalize account emails: scan: %w", err)
		}
		e := r
		e.value = email
		out["email"] = append(out["email"], e)
		if provider.Valid && provider.String != "" {
			p := r
			p.value = provider.String
			out["provider_email"] = append(out["provider_email"], p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("normalize account emails: rows: %w", err)
	}
	return out, nil
}

// outsideASCII is a condition true when column holds a character outside
// printable ASCII, written in each dialect's own syntax. Each compares code
// points: a binary collation on SQL Server, and the regular expressions of
// PostgreSQL and MySQL, whose ranges do not fold accents.
func outsideASCII(engineName, column string) string {
	switch engineName {
	case "mysql":
		return column + ` REGEXP '[^ -~]'`
	case "mssql":
		return column + ` LIKE N'%[^ -~]%' COLLATE Latin1_General_100_BIN2`
	default:
		return column + ` ~ '[^ -~]'`
	}
}

// rewriteAccountAddresses applies the rule to one column's candidates. email
// collides across the install and provider_email within a tenant.
func rewriteAccountAddresses(ctx context.Context, pool DB, column string, rows []accountAddressRow, report *AccountEmailFormReport) error {
	type key struct{ scope, form string }
	groups := map[key][]accountAddressRow{}
	var order []key
	for _, r := range rows {
		k := key{form: core.NormalizeEmail(r.value)}
		if column == "provider_email" {
			k.scope = r.tenant
		}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	for _, k := range order {
		members := groups[k]
		slices.SortFunc(members, func(a, b accountAddressRow) int {
			if c := a.created.Compare(b.created); c != 0 {
				return c
			}
			return cmp.Compare(a.id.String(), b.id.String())
		})
		holder := slices.IndexFunc(members, func(r accountAddressRow) bool { return r.value == k.form })
		if holder < 0 {
			outcome, err := rewriteAccountAddress(ctx, pool, column, members[0], k.form)
			if err != nil {
				return err
			}
			switch outcome {
			case addressChanged:
				// Another writer changed the row since it was read. The next
				// boot reads it again.
				continue
			case addressRefused:
				// An account this run did not read holds the form.
				report.Collisions = append(report.Collisions, collisionOf(column, nil, members))
				continue
			}
			report.Rewritten++
			holder = 0
		}
		if others := slices.Delete(slices.Clone(members), holder, holder+1); len(others) > 0 {
			report.Collisions = append(report.Collisions, collisionOf(column, &members[holder], others))
		}
	}
	return nil
}

// The outcomes of one rewrite.
const (
	addressRewritten = iota
	addressChanged
	addressRefused
)

// rewriteAccountAddress stores form as the account's address in column. It
// names the bytes it replaces, so a row another writer changed since it was
// read is left alone (addressChanged). addressRefused is the unique key
// refusing the form.
func rewriteAccountAddress(ctx context.Context, pool DB, column string, r accountAddressRow, form string) (int, error) {
	res, err := pool.Exec(ctx, `UPDATE sys_users SET `+column+` = $1 WHERE id = $2 AND tenant_id = $3 AND `+column+` = $4`,
		form, r.id, r.tenant, r.value)
	if core.IsDuplicateKey(err) {
		return addressRefused, nil
	}
	if err != nil {
		return 0, fmt.Errorf("normalize account emails: rewrite account %s: %w", r.id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return addressChanged, nil
	}
	return addressRewritten, nil
}

func collisionOf(column string, holder *accountAddressRow, others []accountAddressRow) AccountEmailCollision {
	c := AccountEmailCollision{Column: column}
	var tenants []string
	if holder != nil {
		c.HolderID = holder.id
		tenants = append(tenants, holder.tenant)
	}
	for _, r := range others {
		c.AccountIDs = append(c.AccountIDs, r.id)
		tenants = append(tenants, r.tenant)
	}
	slices.Sort(tenants)
	c.TenantIDs = slices.Compact(tenants)
	return c
}

// logAccountEmailForm writes the run's report. It names ids and counts only.
func logAccountEmailForm(logger *slog.Logger, report AccountEmailFormReport) {
	if logger == nil {
		logger = slog.Default()
	}
	if report.Rewritten > 0 {
		logger.Info("account addresses rewritten to their normalized form", "count", report.Rewritten)
	}
	if len(report.Collisions) == 0 {
		return
	}
	accounts := 0
	for i, c := range report.Collisions {
		accounts += len(c.AccountIDs)
		if i >= accountEmailCollisionLines {
			continue
		}
		ids := make([]string, len(c.AccountIDs))
		for j, id := range c.AccountIDs {
			ids[j] = id.String()
		}
		logger.Warn("accounts hold one address in different forms: the other accounts keep their stored address and cannot sign in by it until an operator decides between them",
			"column", c.Column, "tenant_ids", c.TenantIDs, "holder_id", c.HolderID.String(), "account_ids", ids, "count", len(ids))
	}
	logger.Warn("account address collisions", "collisions", len(report.Collisions), "accounts", accounts)
}
