package runtime

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/ssrf"
)

func TestApplyOutboundAllowance_OpensSharedAddressSpaceOnlyWhenSet(t *testing.T) {
	t.Cleanup(ssrf.ClearAllowlist)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	ssrf.ClearAllowlist()
	require.NoError(t, applyOutboundAllowance(&config.Config{}, logger))
	require.Error(t, ssrf.ValidateURL(ctx, "http://100.100.100.200/"), "the default refuses the range")

	require.NoError(t, applyOutboundAllowance(&config.Config{OutboundAllowCGNAT: true}, logger))
	require.NoError(t, ssrf.ValidateURL(ctx, "http://100.101.102.103/"), "a Tailscale address passes")
	require.NoError(t, ssrf.ValidateURL(ctx, "http://[64:ff9b::6465:6667]/"), "so does its NAT64 form")
	require.Error(t, ssrf.ValidateURL(ctx, "http://10.0.0.1/"), "the allowance opens no other range")
}
