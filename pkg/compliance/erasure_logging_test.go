package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

var errEraseFailed = errors.New("erase failed")

type failingEraser struct{}

func (failingEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	return 0, errEraseFailed
}

type succeedingEraser struct{}

func (succeedingEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	return 3, nil
}

// Erasure exists to remove a subject's data. Writing the identifier that names
// that subject into the log undoes the erasure in the one place the operator is
// least likely to look, and log retention usually outlives the request.
func TestRunSubjectErasure_DoesNotWriteTheSubjectIdentifierToTheLog(t *testing.T) {
	const identifier = "jane.doe@example.com"

	for _, tc := range []struct {
		name   string
		eraser SubjectEraser
	}{
		{"failure path", failingEraser{}},
		{"success path", succeedingEraser{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(restore) })

			reg := &SubjectEraserRegistry{}
			reg.Register(tc.eraser)
			runSubjectErasureOn(context.Background(), reg, identifier)

			out := buf.String()
			if out == "" {
				t.Fatal("expected the orchestration to log something")
			}
			if strings.Contains(out, identifier) {
				t.Errorf("the subject identifier must not appear in the log, got: %s", out)
			}
			if !json.Valid(bytes.TrimSpace(bytes.Split(buf.Bytes(), []byte("\n"))[0])) {
				t.Error("log output must stay structured")
			}
		})
	}
}
