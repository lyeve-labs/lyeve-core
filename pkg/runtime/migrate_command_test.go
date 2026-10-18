package runtime

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMigrateCommand_RefusesIncompleteArgumentsBeforeConnecting(t *testing.T) {
	for name, args := range map[string][]string{
		"no subcommand":      nil,
		"unknown subcommand": {"rollback"},
		"no version":         {"mark-applied", "--plugin", "email"},
		"no plugin":          {"mark-applied", "--version", "002_add_meta"},
		"stray argument":     {"mark-applied", "--plugin", "email", "--version", "002_add_meta", "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "")
			var stdout, stderr bytes.Buffer
			assert.Equal(t, 2, MigrateCommand(args, &stdout, &stderr))
			assert.Contains(t, stderr.String(), "usage: lyeve migrate mark-applied")
			assert.Empty(t, stdout.String())
		})
	}
}
