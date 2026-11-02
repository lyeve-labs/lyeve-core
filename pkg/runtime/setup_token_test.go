package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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

// The slog chain feeds the stored log, the rings and every external sink, so
// the token has to reach the console of the process and nothing that logs.
func TestFirstRunSetupToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		envToken    string
		users       fixedCount
		wantSource  string
		wantPrinted bool
	}{
		{"operator token wins", "operator-setup-token-0123", fixedCount{n: 0}, api.SetupTokenFromEnv, false},
		{"fresh install prints one", "", fixedCount{n: 0}, api.SetupTokenFromLog, true},
		{"install with accounts gets none", "", fixedCount{n: 3}, "", false},
		{"count failure stays closed", "", fixedCount{err: errors.New("db down")}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logged, console bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logged, nil))

			tok := firstRunSetupToken(context.Background(), &config.Config{SetupToken: tc.envToken}, tc.users, logger, &console)

			assert.Equal(t, tc.wantSource, tok.Source())
			assert.NotContains(t, logged.String(), "setup_token", "no token field may reach the log")
			if !tc.wantPrinted {
				assert.Empty(t, console.String())
				return
			}
			printed, ok := strings.CutPrefix(strings.TrimSpace(console.String()), "setup_token=")
			require.True(t, ok, "the console carries the token line")
			require.NotEmpty(t, printed)
			assert.True(t, tok.Matches(printed), "the printed token must be the one setup accepts")
			assert.NotContains(t, logged.String(), printed, "the token itself never reaches the log")
			assert.Contains(t, logged.String(), "standard error", "the log says where the token went")
		})
	}
}
