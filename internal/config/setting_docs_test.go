package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDescribeSetting_ReadsAnySpellingOfTheKey(t *testing.T) {
	for _, k := range []string{"DATABASE_URL", "database_url", "database.url"} {
		d, ok := DescribeSetting(k)
		assert.True(t, ok, k)
		assert.Contains(t, d.Description, "Connection string", k)
	}
	_, ok := DescribeSetting("NO_SUCH_SETTING")
	assert.False(t, ok)
}

// A description is shown to an operator as it is, so it carries no markdown
// the page would print literally, and every key is in the form the resolver
// looks it up by.
func TestSettingDocs_ArePlainTextUnderTheirResolvedNames(t *testing.T) {
	for k, d := range settingDocs {
		assert.Equal(t, normalizeKey(k), k)
		assert.NotEmpty(t, d.Description, k)
		for _, mark := range []string{"`", "](", "<br>"} {
			assert.False(t, strings.Contains(d.Description, mark), "%s carries %q", k, mark)
		}
	}
}
