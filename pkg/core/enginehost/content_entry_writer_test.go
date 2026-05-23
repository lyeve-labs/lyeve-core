package enginehost

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type stubEntryWriter struct{}

func (stubEntryWriter) SaveContentEntry(_ context.Context, _ core.ContentEntryInput) (core.ContentEntrySaved, error) {
	return core.ContentEntrySaved{Outcome: core.ContentEntryCreated}, nil
}

func (stubEntryWriter) DeleteContentEntry(_ context.Context, _ uuid.UUID) error { return nil }

// The entry writer follows the localizer's lifecycle: absent, then the one
// the owning plugin registered, then absent again once it stops, and an
// importing plugin's scoped host sees each step.
func TestEngineHost_ContentEntryWriter_RegisteredByPlugin(t *testing.T) {
	h := &engineHost{}
	var provider core.ContentEntryWriterProvider = h
	if got := provider.ContentEntryWriter(); got != nil {
		t.Fatalf("ContentEntryWriter() before any registration = %v, want nil", got)
	}
	w := stubEntryWriter{}
	owner := core.NewScopedHost(h, "content", core.CapDBWrite|core.CapRoutes)
	consumer := core.NewScopedHost(h, "bulk-import", core.CapRoutes)
	owner.RegisterContentEntryWriter(w)
	if got := consumer.ContentEntryWriter(); got != core.ContentEntryWriter(w) {
		t.Errorf("consumer ContentEntryWriter() = %v, want the writer the owner registered", got)
	}
	owner.RegisterContentEntryWriter(nil)
	if got := consumer.ContentEntryWriter(); got != nil {
		t.Errorf("consumer ContentEntryWriter() after the owner left = %v, want nil", got)
	}
}
