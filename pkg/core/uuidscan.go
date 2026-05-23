package core

import (
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	mssql "github.com/microsoft/go-mssqldb"
)

// UUIDScanner decodes a UUID column into a uuid.UUID on any supported engine.
// Wrap the destination with ScanUUID and pass the result to Row.Scan:
//
//	var id uuid.UUID
//	err := row.Scan(core.ScanUUID(host.Dialect(), &id), &name)
//
// Scanning a UUID column straight into a uuid.UUID is wrong on MSSQL.
// go-mssqldb hands back UNIQUEIDENTIFIER as 16 bytes in Microsoft's
// mixed-endian order, and uuid.UUID.Scan copies those bytes verbatim, so the
// first three groups come back reversed. Writes are unaffected because the
// value is bound as text, which is why the damage shows up later: the id read
// out of a row no longer matches the row it came from, and the next query
// keyed on it silently affects nothing.
type UUIDScanner struct {
	dialect string
	dst     *uuid.UUID
}

var _ sql.Scanner = UUIDScanner{}

// ScanUUID wraps dst so a UUID column read on dialect decodes correctly.
// dialect is the value returned by Host.Dialect: "postgres", "mysql" or
// "mssql". Postgres and MySQL return standard bytes or text and delegate to
// uuid.UUID.Scan unchanged. Only MSSQL needs the byte-order correction.
func ScanUUID(dialect string, dst *uuid.UUID) UUIDScanner {
	return UUIDScanner{dialect: dialect, dst: dst}
}

// Scan implements sql.Scanner. A NULL column yields uuid.Nil.
func (s UUIDScanner) Scan(src any) error {
	if s.dialect != "mssql" {
		return s.dst.Scan(src)
	}
	if src == nil {
		*s.dst = uuid.Nil
		return nil
	}
	var u mssql.UniqueIdentifier
	if err := u.Scan(src); err != nil {
		return fmt.Errorf("scan mssql uuid: %w", err)
	}
	parsed, err := uuid.Parse(u.String())
	if err != nil {
		return fmt.Errorf("parse mssql uuid %q: %w", u.String(), err)
	}
	*s.dst = parsed
	return nil
}

// NullUUIDScanner is the nullable counterpart of UUIDScanner, for a column
// read into a *uuid.UUID.
type NullUUIDScanner struct {
	dialect string
	dst     **uuid.UUID
}

var _ sql.Scanner = NullUUIDScanner{}

// ScanNullUUID wraps a nullable *uuid.UUID target. A NULL column sets the
// pointer to nil. Anything else decodes as ScanUUID does.
func ScanNullUUID(dialect string, dst **uuid.UUID) NullUUIDScanner {
	return NullUUIDScanner{dialect: dialect, dst: dst}
}

// Scan implements sql.Scanner.
func (s NullUUIDScanner) Scan(src any) error {
	if src == nil {
		*s.dst = nil
		return nil
	}
	var u uuid.UUID
	if err := (UUIDScanner{dialect: s.dialect, dst: &u}).Scan(src); err != nil {
		return err
	}
	*s.dst = &u
	return nil
}
