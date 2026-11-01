package testdb

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmssql "github.com/testcontainers/testcontainers-go/modules/mssql"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/lyeve-labs/lyeve-core/internal/testdialect"
)

// The shared containers keep their data directories in memory, so the
// databases every test creates and drops never reach the disk. The request is
// read as the module builds it, and the run stops before any container starts.
func TestPostgresContainer_KeepsItsDataInMemory(t *testing.T) {
	testdialect.Require(t, "postgres")
	tmpfs := requestedTmpfs(t, func(opts ...testcontainers.ContainerCustomizer) error {
		_, err := postgres.Run(context.Background(), "postgres:16-alpine", append(postgresContainerOptions(), opts...)...)
		return err
	})
	require.Contains(t, tmpfs, "/var/lib/postgresql/data")
}

func TestMSSQLContainer_KeepsItsDataInMemory(t *testing.T) {
	testdialect.Require(t, "mssql")
	tmpfs := requestedTmpfs(t, func(opts ...testcontainers.ContainerCustomizer) error {
		_, err := tcmssql.Run(context.Background(), testdialect.MSSQLImage, append(mssqlContainerOptions(), opts...)...)
		return err
	})
	require.Contains(t, tmpfs, "/var/opt/mssql")
}

// requestedTmpfs runs a module with one more option that records the request's
// tmpfs mounts and stops the run.
func requestedTmpfs(t *testing.T, run func(...testcontainers.ContainerCustomizer) error) map[string]string {
	t.Helper()
	stop := errors.New("request read")
	var tmpfs map[string]string
	err := run(testcontainers.CustomizeRequestOption(func(req *testcontainers.GenericContainerRequest) error {
		tmpfs = req.Tmpfs
		return stop
	}))
	require.ErrorIs(t, err, stop)
	return tmpfs
}
