package core

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keepSchemaTransports restores the registry when the test ends, so a
// transport a test registers never reaches the next test.
func keepSchemaTransports(t *testing.T) {
	t.Helper()
	schemaTransports.mu.RLock()
	saved := maps.Clone(schemaTransports.names)
	schemaTransports.mu.RUnlock()
	t.Cleanup(func() {
		schemaTransports.mu.Lock()
		schemaTransports.names = saved
		schemaTransports.mu.Unlock()
	})
}

// registerTransportPlugins registers the GraphQL and gRPC transports the way
// the plugins that serve them do, for one test.
func registerTransportPlugins(t *testing.T) {
	t.Helper()
	keepSchemaTransports(t)
	RegisterSchemaTransport(TransportGraphQL)
	RegisterSchemaTransport(TransportGRPC)
}

// The kernel serves REST and registers no other transport, so a definition
// may name another only in a build that links the plugin serving it.
func TestSchemaTransportNames_KernelKnowsOnlyREST(t *testing.T) {
	assert.Equal(t, []string{"rest"}, SchemaTransportNames())
	require.NoError(t, SchemaTransports{TransportREST: TransportReadOnly}.Validate())
	err := SchemaTransports{TransportGraphQL: TransportOff}.Validate()
	require.Error(t, err)
	assert.Equal(t, `unknown transport "graphql": want "rest"`, err.Error())
}

// The refusal lists every transport a definition may name, REST first and the
// registered names in order, so the author of a typo sees the name they meant.
func TestSchemaTransports_ValidateNamesEveryKnownTransport(t *testing.T) {
	registerTransportPlugins(t)
	err := SchemaTransports{"graphqll": TransportOff}.Validate()
	require.Error(t, err)
	assert.Equal(t, `unknown transport "graphqll": want "rest", "graphql" or "grpc"`, err.Error())
}

// A registered transport is one a definition may name, and the refusal of a
// typo lists it beside the others.
func TestRegisterSchemaTransport_RegisteredNameValidates(t *testing.T) {
	registerTransportPlugins(t)
	require.Error(t, SchemaTransports{"soap": TransportReadOnly}.Validate())

	RegisterSchemaTransport("soap")

	require.NoError(t, SchemaTransports{"soap": TransportReadOnly, TransportREST: TransportOff}.Validate())
	assert.Equal(t, []string{"rest", "graphql", "grpc", "soap"}, SchemaTransportNames())
	err := SchemaTransports{"soapp": TransportOff}.Validate()
	require.Error(t, err)
	assert.Equal(t, `unknown transport "soapp": want "rest", "graphql", "grpc" or "soap"`, err.Error())
}

// A plugin registering a name that is already known, its own or the
// engine's, adds nothing and removes nothing.
func TestRegisterSchemaTransport_KnownNamesChangeNothing(t *testing.T) {
	registerTransportPlugins(t)
	RegisterSchemaTransport(TransportREST)
	RegisterSchemaTransport(TransportGraphQL)
	RegisterSchemaTransport(TransportGraphQL)

	assert.Equal(t, []string{"rest", "graphql", "grpc"}, SchemaTransportNames())
}

func TestRegisterSchemaTransport_RefusesAnEmptyName(t *testing.T) {
	registerTransportPlugins(t)
	assert.Panics(t, func() { RegisterSchemaTransport("") })
	assert.Equal(t, []string{"rest", "graphql", "grpc"}, SchemaTransportNames())
}

// The default is permissive, so a typo that resolved to it would leave a
// schema published over the transport its author meant to close.
func TestSchemaTransports_ValidateRejectsTypos(t *testing.T) {
	registerTransportPlugins(t)
	require.NoError(t, SchemaTransports(nil).Validate())
	require.NoError(t, SchemaTransports{
		TransportREST:    TransportReadWrite,
		TransportGraphQL: TransportReadOnly,
		TransportGRPC:    TransportOff,
	}.Validate())

	err := SchemaTransports{"graphqll": TransportOff}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown transport")

	err = SchemaTransports{TransportGRPC: "readonly"}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mode")

	// An empty mode is a typo too, not an absent key.
	err = SchemaTransports{TransportREST: ""}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mode")
}

func TestQuotedChoices(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{[]string{"rest"}, `"rest"`},
		{[]string{"rest", "grpc"}, `"rest" or "grpc"`},
		{[]string{"rest", "graphql", "grpc"}, `"rest", "graphql" or "grpc"`},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, quotedChoices(tc.names))
	}
}
