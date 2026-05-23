package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// An inner host that provides an engine-bound connection. The connection
// itself is never used, so a nil *sql.Conn with a nil error is enough to tell
// the forward path from the fallback.
type engineConnHost struct {
	*stubHost
	called bool
	err    error
}

func (h *engineConnHost) EngineDBConn(ctx context.Context) (*sql.Conn, error) {
	h.called = true
	return nil, h.err
}

func TestScopedHost_EngineDBConn_ForwardsWhenInnerProvidesOne(t *testing.T) {
	inner := &engineConnHost{stubHost: &stubHost{}}
	sh := NewScopedHost(inner, "schema", CapRawDB)

	if _, err := sh.EngineDBConn(context.Background()); err != nil {
		t.Fatalf("EngineDBConn returned %v, want nil", err)
	}
	if !inner.called {
		t.Fatal("EngineDBConn did not reach the inner host")
	}
}

func TestScopedHost_EngineDBConn_RelaysTheInnerError(t *testing.T) {
	want := errors.New("pool is closed")
	inner := &engineConnHost{stubHost: &stubHost{}, err: want}
	sh := NewScopedHost(inner, "schema", CapRawDB)

	_, err := sh.EngineDBConn(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want it to wrap %v", err, want)
	}
}

// A host with no such connection is a database failure. Falling back to an
// ordinary connection would hand back a tenant-bound one, which is the failure
// this provider exists to prevent, so the refusal has to be an error.
func TestScopedHost_EngineDBConn_RefusesWhenInnerProvidesNone(t *testing.T) {
	sh := NewScopedHost(&stubHost{}, "schema", CapRawDB)

	conn, err := sh.EngineDBConn(context.Background())
	if conn != nil {
		t.Fatal("a host that provides no engine connection must hand out none")
	}
	if err == nil || !strings.Contains(err.Error(), "does not provide one") {
		t.Fatalf("err = %v, want it to say the host provides no engine connection", err)
	}
}
