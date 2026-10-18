package runtime

import (
	"fmt"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/ssrf"
)

// sharedAddressSpace is RFC 6598's range, which the SSRF guard refuses unless
// LYEVE_OUTBOUND_ALLOW_CGNAT is set.
const sharedAddressSpace = "100.64.0.0/10"

// applyOutboundAllowance opens the shared address space to every outbound
// request when the operator set LYEVE_OUTBOUND_ALLOW_CGNAT, as an install
// that reaches its own services over Tailscale does. The allowance is global,
// so each plugin's guard and the address a NAT64 or 6to4 form embeds see it.
func applyOutboundAllowance(cfg *config.Config, logger *slog.Logger) error {
	if !cfg.OutboundAllowCGNAT {
		return nil
	}
	if err := ssrf.AddAllowlist(sharedAddressSpace); err != nil {
		return fmt.Errorf("outbound allowance: %w", err)
	}
	logger.Info("outbound requests may reach the shared address space",
		"range", sharedAddressSpace, "setting", "LYEVE_OUTBOUND_ALLOW_CGNAT")
	return nil
}
