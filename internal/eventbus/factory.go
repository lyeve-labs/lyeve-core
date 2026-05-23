package eventbus

import (
	"fmt"
	"os"
	"strings"
)

// NewFromEnv creates a Publisher based on the EVENT_BUS environment variable.
//
// Supported values:
//   - "" or "noop" -> Noop (default, no external dependency)
//   - "nats", "kafka", "rabbitmq" -> Noop here. A plugin that serves that
//     broker reads the same variable and publishes to the broker itself.
//
// Core owns the in-process bus only, so a broker value is not an error here:
// it is simply not core's to serve.
func NewFromEnv() (Publisher, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("EVENT_BUS")))
	switch backend {
	case "", "noop":
		return Noop{}, nil
	case "nats", "kafka", "rabbitmq":
		// A plugin that serves a broker selects on the same variable, so
		// refusing here would make the value it needs unsettable. The
		// in-process bus stays a noop, and the plugin does the publishing.
		return Noop{}, nil
	default:
		return nil, fmt.Errorf("bus: unknown EVENT_BUS value %q; supported: noop", backend)
	}
}
