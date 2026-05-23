package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

func TestClampLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		limit int
		min   int
		max   int
		want  int
	}{
		{"within range", 25, 1, 200, 25},
		{"below min", 0, 1, 200, 1},
		{"negative", -5, 1, 200, 1},
		{"above max", 1000000, 1, 200, 200},
		{"at max", 200, 1, 200, 200},
		{"at min", 1, 1, 200, 1},
		{"exactly max+1", 201, 1, 200, 200},
		{"custom bounds", 500, 10, 100, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampLimit(tt.limit, tt.min, tt.max)
			if got != tt.want {
				t.Errorf("clampLimit(%d, %d, %d) = %d, want %d",
					tt.limit, tt.min, tt.max, got, tt.want)
			}
		})
	}
}

func TestHTTPStatus(t *testing.T) {
	pgRelationMissing := &pgconn.PgError{Code: "42P01"}

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"ErrNotFound", domain.ErrNotFound, http.StatusNotFound},
		{"ErrNotFound wrapped", fmt.Errorf("store: %w", domain.ErrNotFound), http.StatusNotFound},
		{"ErrConflict", domain.ErrConflict, http.StatusConflict},
		{"ErrConflict wrapped", fmt.Errorf("store: %w", domain.ErrConflict), http.StatusConflict},
		{"ErrForbidden", domain.ErrForbidden, http.StatusForbidden},
		{"ErrForbidden wrapped", fmt.Errorf("store: %w", domain.ErrForbidden), http.StatusForbidden},
		{"ErrUnauth", domain.ErrUnauth, http.StatusUnauthorized},
		{"ErrBadRequest", domain.ErrBadRequest, http.StatusBadRequest},
		{"ErrBadRequest wrapped", fmt.Errorf("%w: unknown filter key %q", domain.ErrBadRequest, "bogus"), http.StatusBadRequest},
		{"pg 42P01 relation missing", pgRelationMissing, http.StatusNotFound},
		{"pg 42P01 wrapped", fmt.Errorf("store: %w", pgRelationMissing), http.StatusNotFound},
		{"generic error", errors.New("boom"), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := httpStatus(tt.err)
			if got != tt.want {
				t.Errorf("httpStatus(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// Verifies that DB error paths in content handlers do not leak raw driver
// error text to API clients. Scans content.go for httpx.ErrorReq calls
// that pass err.Error() instead of a static human-readable message.
func TestContentHandler_NoRawDBErrorsInResponses(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("content.go")
	if err != nil {
		t.Fatalf("read content.go: %v", err)
	}

	lines := strings.Split(string(src), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "httpStatus(err)") && strings.Contains(trimmed, "err.Error()") {
			t.Errorf("content.go:%d: raw driver error text leaked via err.Error() - use a static message instead: %q", i+1, trimmed)
		}
	}
}

// Verifies every store error path in content.go uses a static message,
// not err.Error(). Covers List, GetByID, Insert, Update, Delete and
// SetStatus.
func TestContentHandler_DBErrorMessagesAreStatic(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("content.go")
	if err != nil {
		t.Fatalf("read content.go: %v", err)
	}
	handlersSrc, err := os.ReadFile("content_handlers.go")
	if err != nil {
		t.Fatalf("read content_handlers.go: %v", err)
	}
	relationsSrc, _ := os.ReadFile("content_relations.go")
	lifecycleSrc, _ := os.ReadFile("content_lifecycle.go")

	text := string(src) + string(handlersSrc) + string(relationsSrc) + string(lifecycleSrc)

	staticMessages := []string{
		`"failed to list content"`,
		`"failed to get content"`,
		`"failed to create content"`,
		`"failed to update content"`,
		`"failed to delete content"`,
		`"failed to list related"`,
		`"failed to set relations"`,
		`"failed to publish content"`,
		`"failed to unpublish content"`,
		`"failed to get revision"`,
		`"failed to restore revision"`,
		`"failed to list revisions"`,
		`"bulk insert failed"`,
	}

	for _, msg := range staticMessages {
		if !strings.Contains(text, msg) {
			t.Errorf("expected static error message %s not found in content.go", msg)
		}
	}
}
