package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestSchemaTransports_AbsentIsReadWriteEverywhere(t *testing.T) {
	// A definition written before the field existed decodes to nil, and a nil
	// map must behave as every transport read-write or the upgrade closes
	// surfaces nobody asked to close.
	var nilT core.SchemaTransports
	empty := core.SchemaTransports{}
	partial := core.SchemaTransports{core.TransportGraphQL: core.TransportOff}

	for _, transport := range []string{core.TransportREST, core.TransportGraphQL, core.TransportGRPC} {
		assert.Equal(t, core.TransportReadWrite, nilT.ModeFor(transport), transport)
		assert.True(t, nilT.Serves(transport), transport)
		assert.True(t, nilT.CanRead(transport), transport)
		assert.True(t, nilT.CanWrite(transport), transport)

		assert.Equal(t, core.TransportReadWrite, empty.ModeFor(transport), transport)
		assert.True(t, empty.Serves(transport), transport)
	}

	// A map that names one transport leaves the others at the default.
	assert.False(t, partial.Serves(core.TransportGraphQL))
	assert.True(t, partial.Serves(core.TransportREST))
	assert.True(t, partial.CanWrite(core.TransportGRPC))
}

func TestSchemaTransports_Modes(t *testing.T) {
	tests := []struct {
		name   string
		mode   core.TransportMode
		serves bool
		read   bool
		write  bool
	}{
		{"read write", core.TransportReadWrite, true, true, true},
		{"read only", core.TransportReadOnly, true, true, false},
		{"write only", core.TransportWriteOnly, true, false, true},
		{"off", core.TransportOff, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := core.SchemaTransports{core.TransportGraphQL: tc.mode}
			assert.Equal(t, tc.serves, tr.Serves(core.TransportGraphQL))
			assert.Equal(t, tc.read, tr.CanRead(core.TransportGraphQL))
			assert.Equal(t, tc.write, tr.CanWrite(core.TransportGraphQL))
		})
	}
}
