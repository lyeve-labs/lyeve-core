package core

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
)

// keepResourceSections restores the registry when the test ends, so a
// section a test registers never reaches the next test.
func keepResourceSections(t *testing.T) {
	t.Helper()
	resourceSections.mu.RLock()
	saved := maps.Clone(resourceSections.names)
	resourceSections.mu.RUnlock()
	t.Cleanup(func() {
		resourceSections.mu.Lock()
		resourceSections.names = saved
		resourceSections.mu.Unlock()
	})
}

// The kernel registers no section of its own, so a top-level list in the
// configuration file is a resource section only in a build that links the
// plugin reading it.
func TestResourceSections_KernelRegistersNone(t *testing.T) {
	assert.Empty(t, ResourceSections())
}

// A registered name is listed once, however often it is registered, and the
// list is sorted.
func TestRegisterResourceSection_AddsTheName(t *testing.T) {
	keepResourceSections(t)
	RegisterResourceSection("tasks")
	RegisterResourceSection("jobs")
	RegisterResourceSection("jobs")

	assert.Equal(t, []string{"jobs", "tasks"}, ResourceSections())
}

func TestRegisterResourceSection_RefusesAnEmptyName(t *testing.T) {
	keepResourceSections(t)
	RegisterResourceSection("jobs")
	assert.Panics(t, func() { RegisterResourceSection("") })
	assert.Equal(t, []string{"jobs"}, ResourceSections())
}
