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
	req := requested(t, func(opts ...testcontainers.ContainerCustomizer) error {
		_, err := postgres.Run(context.Background(), "postgres:16-alpine", append(postgresContainerOptions(), opts...)...)
		return err
	})
	require.Contains(t, req.Tmpfs, "/var/lib/postgresql", "the volume the 18 image declares")
	require.Contains(t, req.Tmpfs, "/var/lib/postgresql/data", "the volume the images up to 17 declare")
	require.Equal(t, "/var/lib/postgresql/data", req.Env["PGDATA"], "the data directory is in a mount on every image")
}

func TestMSSQLContainer_KeepsItsDataInMemory(t *testing.T) {
	testdialect.Require(t, "mssql")
	req := requested(t, func(opts ...testcontainers.ContainerCustomizer) error {
		_, err := tcmssql.Run(context.Background(), testdialect.MSSQLImage, append(mssqlContainerOptions(), opts...)...)
		return err
	})
	require.Contains(t, req.Tmpfs, "/var/opt/mssql")
}

// requested runs a module with one more option that records the request and
// stops the run.
func requested(t *testing.T, run func(...testcontainers.ContainerCustomizer) error) testcontainers.GenericContainerRequest {
	t.Helper()
	stop := errors.New("request read")
	var got testcontainers.GenericContainerRequest
	err := run(testcontainers.CustomizeRequestOption(func(req *testcontainers.GenericContainerRequest) error {
		got = *req
		return stop
	}))
	require.ErrorIs(t, err, stop)
	return got
}
