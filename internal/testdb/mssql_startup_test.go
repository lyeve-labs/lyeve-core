package testdb

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmssql "github.com/testcontainers/testcontainers-go/modules/mssql"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lyeve-labs/lyeve-core/internal/testdialect"
)

// The shared SQL Server container waits as long as a loaded host needs for
// recovery. The request is read as the module builds it, and the run stops
// before any container starts.
func TestMSSQLContainer_WaitsLongEnoughForALoadedHost(t *testing.T) {
	testdialect.Require(t, "mssql")
	stop := errors.New("request read")
	var waiting wait.Strategy
	opts := append(mssqlContainerOptions(), testcontainers.CustomizeRequestOption(func(req *testcontainers.GenericContainerRequest) error {
		waiting = req.WaitingFor
		return stop
	}))
	_, err := tcmssql.Run(context.Background(), testdialect.MSSQLImage, opts...)
	require.ErrorIs(t, err, stop)
	require.NotNil(t, waiting)

	// The combined wait holds its bound in an unexported deadline, and each
	// condition in it keeps a timeout of its own.
	all, ok := waiting.(*wait.MultiStrategy)
	require.True(t, ok, "%T", waiting)
	deadline := reflect.ValueOf(all).Elem().FieldByName("deadline")
	require.False(t, deadline.IsNil(), "the combined wait has no deadline")
	require.GreaterOrEqual(t, time.Duration(deadline.Elem().Int()), 3*time.Minute, "the combined wait")
	for _, inner := range all.Strategies {
		timed, ok := inner.(wait.StrategyTimeout)
		require.True(t, ok, "%T sets no timeout", inner)
		require.NotNil(t, timed.Timeout(), "%T falls back to a one-minute default", inner)
		require.GreaterOrEqual(t, *timed.Timeout(), 3*time.Minute, "%T", inner)
	}
}
