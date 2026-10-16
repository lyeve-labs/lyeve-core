package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// sysUserExporter implements compliance.SubjectExporter for the sys_users
// table. Plugins own their own exporters. The account record itself belongs to
// the engine, so nothing registers it unless the engine does. Without it a
// subject who exists in sys_users gets a bundle reporting zero records and
// incomplete=false, which tells them nothing is held about them while their
// account row says otherwise.
type sysUserExporter struct {
	host core.Host
}

// newSysUserExporter constructs a sys_users exporter bound to the engine host,
// which supplies the context-scoped Querier and the dialect at export time.
func newSysUserExporter(host core.Host) *sysUserExporter {
	return &sysUserExporter{host: host}
}

var _ compliance.SubjectExporter = (*sysUserExporter)(nil)

// registerSysUserExporter puts the engine-owned account exporter on the global
// DSAR registry. Lives here rather than in the boot path because runtime.go
// already binds the name "compliance" to the internal controls package.
func registerSysUserExporter(host core.Host) {
	compliance.RegisterSubjectExporter(newSysUserExporter(host))
}

// ExportSubject returns the accounts of the tenant ctx names whose email, id
// or provider address matches identifier, under the "sys_users" section.
// Returns (nil, nil) when nothing matches.
//
// One address can hold an account in several tenants, and the DSAR
// orchestrator runs every exporter once per tenant it covers. Each run
// therefore keeps to its own tenant, or a tenant's export would carry another
// tenant's accounts and a sweep would count each account once per tenant. An
// account stored with no tenant acts in the default tenant, so the default
// tenant's run exports it. A context naming no tenant is refused, because
// the only answer it could give is every tenant's accounts.
//
// password_hash and token_version are withheld: a credential digest is not
// portable subject data and handing it out weakens the account it protects.
func (e *sysUserExporter) ExportSubject(ctx context.Context, identifier string) (map[string]any, error) {
	if strings.TrimSpace(identifier) == "" {
		return nil, nil
	}
	tenant, err := core.RequireTenantID(ctx)
	if err != nil {
		return nil, fmt.Errorf("sys_users export: %w", err)
	}
	tenantCond := "tenant_id = $5"
	if tenant == core.DefaultTenantSlug {
		tenantCond = "tenant_id IN ($5, '')"
	}

	// The id column is UUID on Postgres and a fixed-width string elsewhere, so
	// comparing it to a free-form identifier needs a dialect-aware cast. The
	// same cast the eraser uses, for the same reason.
	var idCast string
	switch e.host.Dialect() {
	case "mysql":
		idCast = "id"
	case "mssql":
		idCast = "CAST(id AS NVARCHAR(36))"
	default:
		idCast = "id::text"
	}

	// anonymized is BOOLEAN on Postgres and TINYINT/BIT on MySQL/MSSQL. A
	// subject erased earlier keeps their id but is marked anonymized. Excluding
	// them here keeps a post-erase export empty instead of echoing the stub.
	boolFalse := "0"
	if e.host.Dialect() == "postgres" {
		boolFalse = "false"
	}

	// SQL selects the candidate accounts and compliance.SameAddress keeps the
	// ones that hold the address, for the reason the eraser gives: under
	// MySQL's and SQL Server's collations a request for an address that holds
	// no account would export the account of its accent or ß variant. An
	// address also matches provider_email, where a tenant-local account keeps
	// the address its provider asserted. The cast id is compared as supplied.
	rows, err := e.host.Querier(ctx).Query(ctx, fmt.Sprintf(`
		SELECT %s, email, roles, tenant_id, disabled, expires_at, created_at, updated_at, provider_email, provider_seen_at,
		       CASE WHEN %s = $4 THEN 1 ELSE 0 END
		FROM sys_users
		WHERE (%s OR %s OR %s = $4) AND anonymized = %s AND %s`,
		idCast, idCast, compliance.AddressCandidates("email", 1), compliance.AddressCandidates("provider_email", 1),
		idCast, boolFalse, tenantCond),
		append(compliance.AddressForms(identifier), identifier, tenant)...)
	if err != nil {
		return nil, fmt.Errorf("sys_users export: %w", err)
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var (
			id, email, tenantID  string
			roles                []string
			disabled             bool
			expiresAt            *time.Time
			createdAt, updatedAt time.Time
			providerEmail        sql.NullString
			providerSeenAt       *time.Time
			byID                 int
		)
		if err := rows.Scan(&id, &email, db.NewRolesScanner(&roles), &tenantID,
			&disabled, &expiresAt, &createdAt, &updatedAt, &providerEmail, &providerSeenAt, &byID); err != nil {
			return nil, fmt.Errorf("sys_users export scan: %w", err)
		}
		if byID == 0 && !compliance.SameAddress(email, identifier) &&
			(!providerEmail.Valid || !compliance.SameAddress(providerEmail.String, identifier)) {
			continue
		}
		if roles == nil {
			roles = []string{}
		}
		row := map[string]any{
			// SQL Server renders a UNIQUEIDENTIFIER cast to text in upper
			// case. Every other answer names an account by the lower case
			// form, so a consumer matching ids as strings finds this one.
			"id":         strings.ToLower(id),
			"email":      email,
			"roles":      roles,
			"tenant_id":  tenantID,
			"disabled":   disabled,
			"created_at": createdAt,
			"updated_at": updatedAt,
		}
		if expiresAt != nil {
			row["expires_at"] = *expiresAt
		}
		if providerEmail.Valid {
			row["provider_email"] = providerEmail.String
		}
		if providerSeenAt != nil {
			row["provider_seen_at"] = *providerSeenAt
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sys_users export: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return map[string]any{"sys_users": out}, nil
}
