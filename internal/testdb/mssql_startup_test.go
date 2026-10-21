package testdb

import (
	"context"
	"errors"
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

	// Each condition keeps a timeout of its own.
	all, ok := waiting.(*wait.MultiStrategy)
	require.True(t, ok, "%T", waiting)
	for _, inner := range all.Strategies {
		timed, ok := inner.(wait.StrategyTimeout)
		require.True(t, ok, "%T sets no timeout", inner)
		require.NotNil(t, timed.Timeout(), "%T falls back to a one-minute default", inner)
		require.GreaterOrEqual(t, *timed.Timeout(), 3*time.Minute, "%T", inner)
	}

	// The combined wait bounds the context every condition runs under. A
	// probe in place of the conditions reads that bound.
	var probe deadlineProbe
	all.Strategies = []wait.Strategy{&probe}
	require.NoError(t, all.WaitUntilReady(context.Background(), nil))
	require.True(t, probe.bounded, "the combined wait has no deadline")
	require.GreaterOrEqual(t, probe.left, 3*time.Minute, "the combined wait")
}

// deadlineProbe is a wait condition that is met at once and records how long
// the context it ran under had left.
type deadlineProbe struct {
	bounded bool
	left    time.Duration
}

func (p *deadlineProbe) WaitUntilReady(ctx context.Context, _ wait.StrategyTarget) error {
	var deadline time.Time
	deadline, p.bounded = ctx.Deadline()
	p.left = time.Until(deadline)
	return nil
}
