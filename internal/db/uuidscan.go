package db

import (
	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// scanUUID wraps dst so a UUID column read on engine decodes correctly. Thin
// alias for core.ScanUUID so the engine and the plugins share one decoder.
// See its docs for why MSSQL needs one.
func scanUUID(engine string, dst *uuid.UUID) core.UUIDScanner {
	return core.ScanUUID(engine, dst)
}

// scanNullUUID wraps a nullable *uuid.UUID target.
func scanNullUUID(engine string, dst **uuid.UUID) core.NullUUIDScanner {
	return core.ScanNullUUID(engine, dst)
}
