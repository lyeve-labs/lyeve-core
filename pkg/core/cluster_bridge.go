package core

import "github.com/lyeve-labs/lyeve-core/pkg/cluster"

// ClusterTransportProvider is an optional interface a plugin implements to
// carry messages between the replicas of one install. The runtime reads it
// after Start() from the one running plugin that implements it, and attaches
// the transport to the relay every subscription was already taken on.
//
// The engine ships no transport of its own, because a store every replica can
// reach is part of the deployment rather than a property of the kernel.
// Without a transport the relay has nothing to publish to, so the process is
// the only replica: a broadcast is a no-op, a subscription never fires, and
// every cache falls back to expiring on its TTL. That is the single-instance
// install, and it is not an error state.
type ClusterTransportProvider interface {
	Plugin

	// ClusterTransport returns the transport, or nil when the plugin holds
	// none. A plugin that could not open its store returns nil rather than an
	// error: the engine keeps running on one replica either way, and the
	// plugin has already said why in its own log.
	ClusterTransport() cluster.Transport
}
