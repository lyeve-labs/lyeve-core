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
// request that goes through pkg/ssrf when the operator set
// LYEVE_OUTBOUND_ALLOW_CGNAT, as an install that reaches its own services over
// Tailscale does. The allowance is global, so each plugin's guard and the
// address a NAT64 or 6to4 form embeds see it. The email plugin asks the same
// guard about a relay or API host its own list refuses, so a provider in the
// range needs no entry in SMTP_ALLOWED_PRIVATE_NETWORKS.
func applyOutboundAllowance(cfg *config.Config, logger *slog.Logger) error {
	if !cfg.OutboundAllowCGNAT {
		return nil
	}
	if err := ssrf.AddAllowlist(sharedAddressSpace); err != nil {
		return fmt.Errorf("outbound allowance: %w", err)
	}
	logger.Info("the outbound guard admits the shared address space",
		"range", sharedAddressSpace, "setting", "LYEVE_OUTBOUND_ALLOW_CGNAT")
	return nil
}
