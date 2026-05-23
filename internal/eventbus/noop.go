package eventbus

import "context"

// Noop is a Publisher that silently discards every message.
// It is the default when no EVENT_BUS backend is configured.
type Noop struct{}

// Publish discards the message and returns nil.
func (Noop) Publish(_ context.Context, _ string, _ []byte) error { return nil }

// Close is a no-op.
func (Noop) Close() error { return nil }
