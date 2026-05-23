package runtime

import (
	"context"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/schemaimport"
)

// reconcileDeclaredSchemas applies the content types declared in the
// configuration tree.
//
// A deployment that keeps its settings in files usually wants its content types
// there too, so the whole project stands up from a repository rather than being
// recreated through the admin UI on every new environment.
//
// Additive by design. A schema in the tree is created or brought up to date. One
// that exists only in the database is left alone, because a file that happens
// not to mention a content type is not an instruction to drop the table and
// everything in it. Removing a content type stays an explicit act through the
// schema API.
//
// A failure here stops the boot. The alternative is an engine serving an API
// whose content types are not the ones its configuration describes, which is
// worse than not starting: the first write would land in whatever shape the
// database happened to already have.
func reconcileDeclaredSchemas(ctx context.Context, eng schemaimport.Engine, configPath string, logger *slog.Logger) error {
	doc, err := config.LoadDeclaredSchemas(configPath)
	if err != nil {
		return err
	}
	if len(doc) == 0 {
		return nil
	}
	bundle, err := schemaimport.ParseBundle(doc)
	if err != nil {
		return err
	}
	if eng == nil {
		logger.Warn("content types are declared in configuration but no schema engine is wired - skipping",
			"schemas", len(bundle.Schemas))
		return nil
	}

	// The boot context names no tenant, and the schema store writes whatever
	// tenant the context carries. Applied bare, every declared content type
	// would land with an empty tenant_id: the tables would exist and the rows
	// would be in the registry, and no tenant-scoped read could see any of them.
	// A deployment that declares its content types in files is the
	// single-tenant case, and that deployment runs as the default tenant, the
	// same one rows with no tenant are attributed to.
	ctx = core.WithTenantID(ctx, core.DefaultTenantSlug)
	plan, err := schemaimport.ApplyBundle(ctx, eng, bundle)
	if err != nil {
		return err
	}
	if plan.Changes() == 0 {
		logger.Debug("declared content types already match", "schemas", len(bundle.Schemas))
		return nil
	}

	created, updated := 0, 0
	for _, s := range plan.Schemas {
		switch s.Action {
		case schemaimport.ActionCreate:
			created++
		case schemaimport.ActionUpdate:
			updated++
		}
	}
	logger.Info("declared content types applied",
		"created", created, "updated", updated, "total", len(bundle.Schemas))
	return nil
}
