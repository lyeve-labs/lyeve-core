//go:build !mutest

package db

import (
	"testing"

	"github.com/google/uuid"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mssqlWire converts a uuid.UUID to the mixed-endian []byte that go-mssqldb
// returns for a UNIQUEIDENTIFIER column, by round-tripping through the
// driver's own Value() method.
func mssqlWire(u uuid.UUID) []byte {
	var ms mssql.UniqueIdentifier
	copy(ms[:], u[:])
	b, _ := ms.Value()
	return b.([]byte)
}

func TestScanUUID_Dialects(t *testing.T) {
	uid := uuid.MustParse("d7e8f9a0-b1c2-4d3e-5f67-89abcdef0123")

	tests := []struct {
		name   string
		engine string
		src    any
	}{
		// Postgres (pgx): driver returns 16 raw bytes in standard order.
		{"postgres raw bytes", "postgres", uid[:]},
		{"postgres string", "postgres", uid.String()},
		// MySQL (go-sql-driver): driver returns 36-char text for CHAR(36).
		{"mysql string", "mysql", uid.String()},
		{"mysql text bytes", "mysql", []byte(uid.String())},
		// MSSQL (go-mssqldb): driver returns 16 mixed-endian bytes.
		{"mssql wire bytes", "mssql", mssqlWire(uid)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got uuid.UUID
			err := scanUUID(tt.engine, &got).Scan(tt.src)
			require.NoError(t, err)
			assert.Equal(t, uid, got)
		})
	}
}

func TestScanNullUUID_Dialects(t *testing.T) {
	uid := uuid.MustParse("abcdef01-2345-6789-abcd-ef0123456789")

	tests := []struct {
		name   string
		engine string
		src    any
		want   *uuid.UUID // nil means expect nil
	}{
		{"postgres value", "postgres", uid[:], &uid},
		{"mysql value", "mysql", uid.String(), &uid},
		{"mssql value", "mssql", mssqlWire(uid), &uid},
		{"postgres null", "postgres", nil, nil},
		{"mysql null", "mysql", nil, nil},
		{"mssql null", "mssql", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dst *uuid.UUID
			err := scanNullUUID(tt.engine, &dst).Scan(tt.src)
			require.NoError(t, err)
			if tt.want == nil {
				assert.Nil(t, dst)
			} else {
				require.NotNil(t, dst)
				assert.Equal(t, *tt.want, *dst)
			}
		})
	}
}

// TestUUIDRoundtrip_Dialects validates that a UUID survives a full encode-
// decode cycle per engine: standard bytes are converted to each engine's
// wire representation, scanned back through scanUUID, and verified byte-
// for-byte against the original.
func TestUUIDRoundtrip_Dialects(t *testing.T) {
	uid := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

	tests := []struct {
		name   string
		engine string
		encode func(uuid.UUID) any
	}{
		// pgx returns UUID as 16 raw bytes in standard order.
		{"postgres", "postgres", func(u uuid.UUID) any { return u[:] }},
		// go-sql-driver/mysql returns CHAR(36) as 36-char text.
		{"mysql", "mysql", func(u uuid.UUID) any { return u.String() }},
		// go-mssqldb returns UNIQUEIDENTIFIER as 16 mixed-endian bytes.
		{"mssql", "mssql", func(u uuid.UUID) any { return mssqlWire(u) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire := tt.encode(uid)
			var got uuid.UUID
			err := scanUUID(tt.engine, &got).Scan(wire)
			require.NoError(t, err)
			assert.Equal(t, uid, got,
				"roundtrip must preserve UUID bytes; engine=%s", tt.engine)
		})
	}
}
