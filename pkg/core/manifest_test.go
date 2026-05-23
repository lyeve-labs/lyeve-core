package core

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeManifest_KeepsAWellFormedManifest(t *testing.T) {
	m := PluginManifest{
		Label:       "Search",
		Description: "Ranked full-text search across every schema.",
		Category:    CategoryContent,
		Maturity:    MaturityStable,
	}
	assert.Equal(t, m, NormalizeManifest(m))
}

func TestNormalizeManifest_TrimsCapsAndDrops(t *testing.T) {
	cases := []struct {
		name string
		in   PluginManifest
		want PluginManifest
	}{
		{
			name: "space around the text",
			in:   PluginManifest{Label: "  Search\n", Description: "\tFinds entries. "},
			want: PluginManifest{Label: "Search", Description: "Finds entries."},
		},
		{
			name: "a label at the cap",
			in:   PluginManifest{Label: strings.Repeat("a", ManifestLabelMax)},
			want: PluginManifest{Label: strings.Repeat("a", ManifestLabelMax)},
		},
		{
			name: "a label past the cap",
			in:   PluginManifest{Label: strings.Repeat("a", ManifestLabelMax+1)},
			want: PluginManifest{Label: strings.Repeat("a", ManifestLabelMax)},
		},
		{
			name: "a cap counts runes, not bytes",
			in:   PluginManifest{Label: strings.Repeat("é", ManifestLabelMax+5)},
			want: PluginManifest{Label: strings.Repeat("é", ManifestLabelMax)},
		},
		{
			name: "a cut that ends on a space",
			in:   PluginManifest{Label: strings.Repeat("a", ManifestLabelMax-1) + " and more"},
			want: PluginManifest{Label: strings.Repeat("a", ManifestLabelMax-1)},
		},
		{
			name: "a description past the cap",
			in:   PluginManifest{Label: "X", Description: strings.Repeat("d", ManifestDescriptionMax+10)},
			want: PluginManifest{Label: "X", Description: strings.Repeat("d", ManifestDescriptionMax)},
		},
		{
			name: "a category outside the set",
			in:   PluginManifest{Label: "X", Category: "Content", Maturity: MaturityBeta},
			want: PluginManifest{Label: "X", Maturity: MaturityBeta},
		},
		{
			name: "a maturity outside the set",
			in:   PluginManifest{Label: "X", Category: CategoryAI, Maturity: "alpha"},
			want: PluginManifest{Label: "X", Category: CategoryAI},
		},
		{
			name: "nothing at all",
			in:   PluginManifest{},
			want: PluginManifest{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeManifest(c.in)
			assert.Equal(t, c.want, got)
			assert.LessOrEqual(t, utf8.RuneCountInString(got.Label), ManifestLabelMax)
			assert.LessOrEqual(t, utf8.RuneCountInString(got.Description), ManifestDescriptionMax)
			assert.Equal(t, got, NormalizeManifest(got), "normalizing twice changes nothing more")
		})
	}
}

func TestPluginCategories_AreTheClosedSet(t *testing.T) {
	want := []PluginCategory{"content", "delivery", "access", "automation", "insight", "operations", "compliance", "ai", "platform"}
	got := PluginCategories()
	assert.Equal(t, want, got)
	for _, c := range got {
		assert.True(t, c.Known(), "%s", c)
	}
	assert.False(t, PluginCategory("").Known())
	assert.False(t, PluginCategory("Content").Known())

	got[0] = "changed"
	assert.Equal(t, want, PluginCategories(), "the set is handed out as a copy")
}

func TestPluginMaturity_Known(t *testing.T) {
	assert.True(t, MaturityStable.Known())
	assert.True(t, MaturityBeta.Known())
	assert.False(t, PluginMaturity("").Known())
	assert.False(t, PluginMaturity("alpha").Known())
}
