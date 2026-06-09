package compliance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A super_admin holding no tenant claim reaches every exporter on the empty
// scope and gets nothing back. An export that reached no tenant must say so,
// or a controller answering an Art.15 request is handed an empty export that
// says it is complete.
func TestRunSubjectExport_UnscopedCoversNothingAndSaysSo(t *testing.T) {
	res, err := RunSubjectExport(context.Background(), "person@example.com")
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, 1, res.Scope.Requested)
	assert.Equal(t, 0, res.Scope.Covered, "an export that reached no tenant reported coverage")
	assert.Empty(t, res.Scope.Tenants, "a tenant was listed that the export never reached")
	assert.True(t, res.Incomplete, "an export that covered nothing reported itself complete")
}

// The ordinary path is unchanged: a scoped caller covers the one tenant it
// named, and the bundle is complete unless an exporter failed.
func TestRunSubjectExport_ScopedCoversItsTenant(t *testing.T) {
	ctx := core.WithTenantID(context.Background(), "acme")

	res, err := RunSubjectExport(ctx, "person@example.com")
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, 1, res.Scope.Requested)
	assert.Equal(t, 1, res.Scope.Covered)
	assert.Equal(t, []string{"acme"}, res.Scope.Tenants)
	assert.False(t, res.Incomplete, "a scoped export with no exporter errors reported itself incomplete")
}
