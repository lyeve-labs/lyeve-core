package runtime

import (
	"context"
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

// ExportSubject returns the account record whose email or id matches
// identifier, under the "sys_users" section. Returns (nil, nil) when nothing
// matches.
//
// password_hash and token_version are withheld: a credential digest is not
// portable subject data and handing it out weakens the account it protects.
func (e *sysUserExporter) ExportSubject(ctx context.Context, identifier string) (map[string]any, error) {
	if strings.TrimSpace(identifier) == "" {
		return nil, nil
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

	// The email and id halves take separate parameters for the reason the
	// eraser gives: stored addresses are lower case, so a subject request
	// naming Foo@x.com has to be folded before it can match, while the cast id
	// is compared as supplied.
	rows, err := e.host.Querier(ctx).Query(ctx, fmt.Sprintf(`
		SELECT %s, email, roles, tenant_id, disabled, expires_at, created_at, updated_at
		FROM sys_users
		WHERE (email = $1 OR %s = $2) AND anonymized = %s`, idCast, idCast, boolFalse),
		core.NormalizeEmail(identifier), identifier)
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
		)
		if err := rows.Scan(&id, &email, db.NewRolesScanner(&roles), &tenantID,
			&disabled, &expiresAt, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("sys_users export scan: %w", err)
		}
		if roles == nil {
			roles = []string{}
		}
		row := map[string]any{
			"id":         id,
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
