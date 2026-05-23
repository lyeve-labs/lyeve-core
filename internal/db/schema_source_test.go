package db

import (
	"context"
	"errors"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// schemaSourceStub is a SchemaSource that is deliberately not a SchemaStore.
// The content path has to work against any implementation, because the
// definitions will not always live behind this package's own store.
type schemaSourceStub struct {
	asked string
	err   error
}

func (s *schemaSourceStub) GetByName(_ context.Context, name string) (*core.Schema, error) {
	s.asked = name
	return nil, s.err
}

func (s *schemaSourceStub) List(context.Context) ([]*core.Schema, error) {
	return nil, s.err
}

func TestContentStore_ResolvesSchemaThroughTheInjectedSource(t *testing.T) {
	want := errors.New("the source was asked")
	stub := &schemaSourceStub{err: want}

	_, _, err := NewContentStore(nil, stub).table(context.Background(), "articles")

	if !errors.Is(err, want) {
		t.Fatalf("table() error = %v, want the source's own error %v", err, want)
	}
	if stub.asked != "articles" {
		t.Fatalf("source was asked for %q, want %q", stub.asked, "articles")
	}
}
