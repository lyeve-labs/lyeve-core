package core

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

// A schema is reachable over the engine's own content API and over every
// transport a plugin registers with RegisterSchemaTransport. Which of them
// serve a given schema, and whether they may write through it, is part of the
// schema definition rather than a property of the instance, because
// activating a transport plugin is an instance-wide decision and publishing a
// content type is not.
//
// This type lives in pkg/core rather than internal/domain because the
// transport plugins are its primary consumers and a plugin may not import
// internal. A definition may name GraphQL or gRPC once the plugin that serves
// the transport has registered its name.
const (
	// TransportREST is the engine's content API on /api/v1. The admin API is
	// not a transport and is never gated by this.
	TransportREST = "rest"
	// TransportGraphQL is the GraphQL endpoint a transport plugin serves.
	TransportGraphQL = "graphql"
	// TransportGRPC is the gRPC content and schema services a transport
	// plugin serves.
	TransportGRPC = "grpc"
)

// TransportMode says how far a transport may reach into a schema.
type TransportMode string

const (
	// TransportReadWrite serves the schema and accepts writes through it.
	TransportReadWrite TransportMode = "rw"
	// TransportReadOnly serves the schema but refuses writes. A GraphQL
	// schema in this mode gets query and subscription fields and no mutation.
	TransportReadOnly TransportMode = "r"
	// TransportWriteOnly accepts writes and serves no reads.
	TransportWriteOnly TransportMode = "w"
	// TransportOff does not serve the schema at all. The schema has no
	// GraphQL type and no gRPC service method, not an empty one.
	TransportOff TransportMode = "off"
)

// DefaultTransportMode is what an unnamed transport resolves to. Absent means
// everything, so a definition that never mentions a transport is served
// read-write on every transport.
const DefaultTransportMode = TransportReadWrite

// SchemaTransports maps a transport name to the mode it serves a schema in.
// A nil or empty map means every transport is read-write, so the zero value
// is the permissive default rather than a closed door.
type SchemaTransports map[string]TransportMode

// ModeFor resolves the mode for one transport. An absent key is
// DefaultTransportMode, which is what makes the field optional.
func (t SchemaTransports) ModeFor(transport string) TransportMode {
	if t == nil {
		return DefaultTransportMode
	}
	m, ok := t[transport]
	if !ok {
		return DefaultTransportMode
	}
	return m
}

// Serves reports whether the transport exposes the schema in any form. A
// transport that serves nothing should omit the schema entirely rather than
// publish a type whose every operation fails.
func (t SchemaTransports) Serves(transport string) bool {
	return t.ModeFor(transport) != TransportOff
}

// CanRead reports whether the transport may read the schema.
func (t SchemaTransports) CanRead(transport string) bool {
	m := t.ModeFor(transport)
	return m == TransportReadWrite || m == TransportReadOnly
}

// CanWrite reports whether the transport may write the schema.
func (t SchemaTransports) CanWrite(transport string) bool {
	m := t.ModeFor(transport)
	return m == TransportReadWrite || m == TransportWriteOnly
}

// Validate rejects an unknown transport name or mode. A typo must not resolve
// to the default, because the default is permissive and the writer's intent
// was to restrict: "graphqll": "off" that is silently ignored leaves the
// schema published over GraphQL and reads, in the definition, as closed. A
// name is known when it is REST or a registered transport.
func (t SchemaTransports) Validate() error {
	for name, mode := range t {
		if !isSchemaTransport(name) {
			return fmt.Errorf("unknown transport %q: want %s", name, quotedChoices(SchemaTransportNames()))
		}
		switch mode {
		case TransportReadWrite, TransportReadOnly, TransportWriteOnly, TransportOff:
		default:
			return fmt.Errorf("transport %q: unknown mode %q: want %q, %q, %q or %q",
				name, mode, TransportReadWrite, TransportReadOnly, TransportWriteOnly, TransportOff)
		}
	}
	return nil
}

// schemaTransports holds the registered transport names. REST is the
// engine's own and is never held here. Every other name arrives from the
// plugin that serves the transport.
var schemaTransports = struct {
	mu    sync.RWMutex
	names map[string]bool
}{names: map[string]bool{}}

// RegisterSchemaTransport adds a transport a schema definition may name in
// its transports map. A plugin that serves schemas over a transport of its
// own registers the name from init, before any definition is validated.
// Registering a known name changes nothing. It panics on an empty name,
// which no definition could use.
func RegisterSchemaTransport(name string) {
	if name == "" {
		panic("core.RegisterSchemaTransport: name is empty")
	}
	if name == TransportREST {
		return
	}
	schemaTransports.mu.Lock()
	defer schemaTransports.mu.Unlock()
	schemaTransports.names[name] = true
}

// SchemaTransportNames returns every transport a definition may name: REST
// first, as the engine's own, then the registered names in sorted order.
func SchemaTransportNames() []string {
	schemaTransports.mu.RLock()
	defer schemaTransports.mu.RUnlock()
	registered := make([]string, 0, len(schemaTransports.names))
	for name := range schemaTransports.names {
		registered = append(registered, name)
	}
	slices.Sort(registered)
	return append([]string{TransportREST}, registered...)
}

func isSchemaTransport(name string) bool {
	if name == TransportREST {
		return true
	}
	schemaTransports.mu.RLock()
	defer schemaTransports.mu.RUnlock()
	return schemaTransports.names[name]
}

// quotedChoices renders names as a choice for an error message:
// "a", "b" or "c".
func quotedChoices(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	if len(quoted) < 2 {
		return strings.Join(quoted, "")
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}
