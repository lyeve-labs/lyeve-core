package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// PluginCategory is where the admin console files a plugin. The set is
// closed, so every console groups plugins the same way, and a value outside
// it is dropped rather than shown.
type PluginCategory string

// The categories a plugin may name.
const (
	CategoryContent    PluginCategory = "content"
	CategoryDelivery   PluginCategory = "delivery"
	CategoryAccess     PluginCategory = "access"
	CategoryAutomation PluginCategory = "automation"
	CategoryInsight    PluginCategory = "insight"
	CategoryOperations PluginCategory = "operations"
	CategoryCompliance PluginCategory = "compliance"
	CategoryAI         PluginCategory = "ai"
	CategoryPlatform   PluginCategory = "platform"
)

// PluginMaturity says how settled a plugin is. A console marks a beta plugin
// as one.
type PluginMaturity string

// The maturities a plugin may name.
const (
	MaturityStable PluginMaturity = "stable"
	MaturityBeta   PluginMaturity = "beta"
)

// The most runes each text of a manifest keeps. A console lays a label out on
// one line and a description on a few, so a longer text is cut rather than
// left to break the layout.
const (
	ManifestLabelMax       = 40
	ManifestDescriptionMax = 240
)

// PluginManifest is how a plugin describes itself to the person running the
// install. The plugin status carries it, and the admin console lists the
// plugin under its label, in its category. It carries no license terms,
// because what an install may run is the licensing implementation's to say.
type PluginManifest struct {
	// Label is the plugin's name as a person reads it, such as "Search".
	Label string `json:"label"`
	// Description says what the plugin does, in a sentence or two.
	Description string `json:"description,omitempty"`
	// Category is one of the Category constants.
	Category PluginCategory `json:"category,omitempty"`
	// Maturity is MaturityStable or MaturityBeta.
	Maturity PluginMaturity `json:"maturity,omitempty"`
}

// Describer is a plugin that describes itself. A plugin without it is listed
// by its name.
type Describer interface {
	Manifest() PluginManifest
}

// pluginCategories is the closed set, in the order a console lists it.
var pluginCategories = []PluginCategory{
	CategoryContent, CategoryDelivery, CategoryAccess, CategoryAutomation, CategoryInsight,
	CategoryOperations, CategoryCompliance, CategoryAI, CategoryPlatform,
}

// PluginCategories returns the closed set of categories. The slice is a copy,
// so changing it changes nothing.
func PluginCategories() []PluginCategory {
	return append([]PluginCategory(nil), pluginCategories...)
}

// Known reports whether c is in the closed set.
func (c PluginCategory) Known() bool {
	for _, k := range pluginCategories {
		if c == k {
			return true
		}
	}
	return false
}

// Known reports whether m is MaturityStable or MaturityBeta.
func (m PluginMaturity) Known() bool {
	return m == MaturityStable || m == MaturityBeta
}

// NormalizeManifest trims the text, caps the label at 40 runes and the
// description at 240, and drops an unknown category or maturity.
func NormalizeManifest(m PluginManifest) PluginManifest {
	m.Label = capRunes(strings.TrimSpace(m.Label), ManifestLabelMax)
	m.Description = capRunes(strings.TrimSpace(m.Description), ManifestDescriptionMax)
	if !m.Category.Known() {
		m.Category = ""
	}
	if !m.Maturity.Known() {
		m.Maturity = ""
	}
	return m
}

// capRunes keeps the first limit runes of s, without the space a cut can
// leave at the end.
func capRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return strings.TrimRightFunc(s[:i], unicode.IsSpace)
		}
		n++
	}
	return s
}
