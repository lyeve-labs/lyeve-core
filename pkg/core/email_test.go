package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeEmail_OneFormForEverySpelling(t *testing.T) {
	const composed = "josé@example.com"
	for _, spelling := range []string{
		composed,
		"josé@example.com",
		"JOSÉ@Example.com",
		" JosÉ@example.com\t",
	} {
		assert.Equal(t, composed, NormalizeEmail(spelling), "%q", spelling)
	}
	assert.NotEqual(t, NormalizeEmail("jose@example.com"), NormalizeEmail(composed), "an accent is part of the address")
	assert.NotEqual(t, NormalizeEmail("ſam@example.com"), NormalizeEmail("sam@example.com"), "lowering is all it folds")
}

func TestNormalizeEmail_IsStable(t *testing.T) {
	for _, spelling := range []string{
		"josé@example.com",
		"İSTANBUL@example.com",
		"K@example.com",
		"STRAẞE@example.com",
		"zoë-josé@example.com",
		"H\u0331@example.com",
	} {
		once := NormalizeEmail(spelling)
		assert.Equal(t, once, NormalizeEmail(once), "%q", spelling)
	}
}

func TestValidEmail_OnePlainAddress(t *testing.T) {
	for _, ok := range []string{"person@example.com", "jo.hn@example.com", "admin@localhost", "jos\u00e9@example.com"} {
		assert.True(t, ValidEmail(ok), "%q", ok)
	}
	for _, bad := range []string{"", "person", "@example.com", "Person <person@example.com>", "<person@example.com>",
		"person@example.com (comment)", "per son@example.com", `"person"@example.com`,
		".person@example.com", "person.@example.com", "per..son@example.com", "person@.example.com", "person@example.com.", "person@example..com"} {
		assert.False(t, ValidEmail(bad), "%q", bad)
	}
}
