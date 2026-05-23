// Package eventbus provides a pluggable message-bus abstraction.
//
// The CMS publishes one event per content lifecycle operation (create, update,
// delete) to the configured backend.
//
// In-core, only the noop backend is supported: events are discarded with no
// external dependency required. A broker backend (NATS, Kafka, RabbitMQ)
// comes from a plugin that serves it.
//
// Select the backend at startup via the EVENT_BUS environment variable.
// Supported values: "" or "noop". "nats", "kafka" and "rabbitmq" are accepted
// as well and left to a plugin that serves that broker.
package eventbus

import "context"

// Publisher is the interface all bus backends implement.
type Publisher interface {
	// Publish serializes and sends payload to the given topic.
	// Implementations must be safe for concurrent use from multiple goroutines.
	Publish(ctx context.Context, topic string, payload []byte) error

	// Close flushes any pending messages and releases resources.
	Close() error
}
