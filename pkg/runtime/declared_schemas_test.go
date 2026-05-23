package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// tenantRecordingEngine is the smallest schemaimport.Engine that lets a test
// see which tenant an Apply arrived under.
type tenantRecordingEngine struct {
	applied map[string]string
}

func (e *tenantRecordingEngine) Apply(ctx context.Context, name string, _ json.RawMessage) error {
	e.applied[name] = core.TenantIDFromCtx(ctx)
	return nil
}

func (e *tenantRecordingEngine) PreviewDDL(context.Context, string, json.RawMessage) ([]core.DDLStatement, error) {
	return nil, nil
}

func (e *tenantRecordingEngine) Get(context.Context, string) (json.RawMessage, error) {
	return nil, os.ErrNotExist
}

func (e *tenantRecordingEngine) List(context.Context) ([]json.RawMessage, error) {
	return nil, nil
}

// A content type declared in configuration has to belong to the default
// tenant. The boot context carries none, and a schema applied under no tenant
// is stored under no tenant, where no tenant-scoped read can find it.
func TestReconcileDeclaredSchemas_AppliesUnderTheDefaultTenant(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "lyeve.yaml")
	require.NoError(t, os.WriteFile(tree, []byte(`
schemas:
  - name: article
    display_name: Article
    fields:
      - name: title
        field_type: text
        required: true
`), 0o600))

	eng := &tenantRecordingEngine{applied: map[string]string{}}
	err := reconcileDeclaredSchemas(context.Background(), eng, tree, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)

	require.Equal(t, map[string]string{"article": core.DefaultTenantSlug}, eng.applied)
}
