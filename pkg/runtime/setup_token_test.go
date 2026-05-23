package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
)

type fixedCount struct {
	n   int64
	err error
}

func (f fixedCount) Count(context.Context) (int64, error) { return f.n, f.err }

func TestFirstRunSetupToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		envToken   string
		users      fixedCount
		wantSource string
		wantLogged bool
	}{
		{"operator token wins", "operator-setup-token-0123", fixedCount{n: 0}, api.SetupTokenFromEnv, false},
		{"fresh install prints one", "", fixedCount{n: 0}, api.SetupTokenFromLog, true},
		{"install with accounts gets none", "", fixedCount{n: 3}, "", false},
		{"count failure stays closed", "", fixedCount{err: errors.New("db down")}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))

			tok := firstRunSetupToken(context.Background(), &config.Config{SetupToken: tc.envToken}, tc.users, logger)

			assert.Equal(t, tc.wantSource, tok.Source())
			var printed string
			for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
				var rec map[string]any
				if json.Unmarshal(line, &rec) == nil {
					if v, ok := rec["setup_token"].(string); ok {
						printed = v
					}
				}
			}
			if !tc.wantLogged {
				assert.Empty(t, printed, "no token may reach the log")
				return
			}
			require.NotEmpty(t, printed)
			assert.True(t, tok.Matches(printed), "the printed token must be the one setup accepts")
		})
	}
}
