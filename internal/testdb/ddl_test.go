package testdb

import (
	"errors"
	"fmt"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
)

// ownerEntries returns the registrations made under owner.
func ownerEntries(owner string) []ddlEntry {
	var out []ddlEntry
	for _, e := range registeredDDLSince(0) {
		if e.owner == owner {
			out = append(out, e)
		}
	}
	return out
}

// The first registration under an owner is the one kept. The statements are
// never run here: both return one only on a dialect that does not exist.
func TestRegisterDDL_AnOwnerRegistersOnce(t *testing.T) {
	first := func(dialect string) []string {
		if dialect == "none" {
			return []string{"first"}
		}
		return nil
	}
	second := func(dialect string) []string {
		if dialect == "none" {
			return []string{"second"}
		}
		return nil
	}

	RegisterDDL("probe-once", first)
	RegisterDDL("probe-once", second)

	entries := ownerEntries("probe-once")
	if assert.Len(t, entries, 1) {
		assert.Equal(t, []string{"first"}, entries[0].ddl("none"))
	}
}

func TestRegisterDDL_RefusesAnEmptyOwnerOrNoStatements(t *testing.T) {
	none := func(string) []string { return nil }
	assert.Panics(t, func() { RegisterDDL("", none) })
	assert.Panics(t, func() { RegisterDDL("probe-nil", nil) })
	assert.Empty(t, ownerEntries(""))
	assert.Empty(t, ownerEntries("probe-nil"))
}

// Only MySQL's duplicate index name is skipped. A duplicate table or any
// other failure still fails the database it was meant for.
func TestIsDuplicateIndexName(t *testing.T) {
	dup := &mysqldriver.MySQLError{Number: 1061, Message: "Duplicate key name 'idx'"}
	assert.True(t, isDuplicateIndexName(dup))
	assert.True(t, isDuplicateIndexName(fmt.Errorf("exec: %w", dup)))
	assert.False(t, isDuplicateIndexName(&mysqldriver.MySQLError{Number: 1050, Message: "Table exists"}))
	assert.False(t, isDuplicateIndexName(errors.New("Duplicate key name 'idx'")))
}
