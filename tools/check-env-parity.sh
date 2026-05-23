#!/usr/bin/env bash
# check-env-parity.sh - fail if any variable config.go reads is absent from
# .env.example.
#
# lookup() is the layered resolver every setting goes through: an environment
# variable, then the YAML tree, then the admin layer. This matches it as well as
# os.Getenv, envOr and envBool.
#
# Also reports vars in .env.example that config.go does not read
# (informational, non-failing). Another package in the module may read them.
#
# Usage:
#   tools/check-env-parity.sh         # check mode (fails on drift)
#   tools/check-env-parity.sh diff    # show diff only (no exit code)

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG_FILE="$ROOT_DIR/internal/config/config.go"
ENV_EXAMPLE="$ROOT_DIR/.env.example"

MODE="${1:-check}"

if [ ! -f "$CONFIG_FILE" ]; then
  echo "::error::config.go not found at $CONFIG_FILE"
  exit 1
fi
if [ ! -f "$ENV_EXAMPLE" ]; then
  echo "::error::.env.example not found at $ENV_EXAMPLE"
  exit 1
fi

# Extract all env var names from config.go.
# Patterns: lookup("VAR"), os.Getenv("VAR"), envOr("VAR"), envBool("VAR")
# Only extract from actual code lines (not comments or string literals).
CODE_VARS=$(
  grep -n 'lookup(\|os\.Getenv\|envOr(\|envBool(' "$CONFIG_FILE" \
    | grep -v '^\s*//' \
    | sed -n 's/.*"\([A-Z_][A-Z_0-9]*\)".*/\1/p' \
    | grep -v '^PATH$' \
    | sort -u
)

# Extract all env var names from .env.example.
# Match: VAR=value or #VAR=value (commented out).
EXAMPLE_VARS=$(
  sed -n 's/^\([A-Z_][A-Z_0-9]*\)=.*/\1/p;s/^#\([A-Z_][A-Z_0-9]*\)=.*/\1/p' "$ENV_EXAMPLE" | sort -u
)

# Find vars in code but missing from .env.example
MISSING=""
while IFS= read -r var; do
  if [ -z "$var" ]; then continue; fi
  # A herestring, not a pipe. grep -q exits on its first match and closes the
  # pipe under the writer, which kills echo with SIGPIPE. Then pipefail reports
  # the writer's 141 rather than grep's 0, and a variable that IS present is
  # recorded as missing. Whether echo finishes first is a scheduling race, so it
  # passes on a workstation and fails on a loaded runner.
  if ! grep -qxF "$var" <<< "$EXAMPLE_VARS"; then
    MISSING="$MISSING$var"$'\n'
  fi
done <<< "$CODE_VARS"

# Find vars in .env.example that config.go does not read
DEAD=""
while IFS= read -r var; do
  if [ -z "$var" ]; then continue; fi
  if ! grep -qxF "$var" <<< "$CODE_VARS"; then
    DEAD="$DEAD$var"$'\n'
  fi
done <<< "$EXAMPLE_VARS"

MISSING=$(echo "$MISSING" | sed '/^$/d' | sort -u)
DEAD=$(echo "$DEAD" | sed '/^$/d' | sort -u)

CODE_COUNT=$(echo "$CODE_VARS" | wc -l | tr -d ' ')
EXAMPLE_COUNT=$(echo "$EXAMPLE_VARS" | wc -l | tr -d ' ')
MISSING_COUNT=$(echo "$MISSING" | wc -l | tr -d ' ')
DEAD_COUNT=$(echo "$DEAD" | wc -l | tr -d ' ')

# For CI output, ensure zero-length counts show as 0
[ -z "$MISSING" ] && MISSING_COUNT=0
[ -z "$DEAD" ] && DEAD_COUNT=0

echo "=== Environment Variable Parity ==="
echo "  config.go vars:  $CODE_COUNT"
echo "  .env.example vars: $EXAMPLE_COUNT"
echo ""

if [ "$MISSING_COUNT" -gt 0 ]; then
  echo "::warning::MISSING from .env.example ($MISSING_COUNT):"
  echo "$MISSING" | while IFS= read -r line; do
    echo "  - $line"
  done
  echo ""
fi

if [ "$DEAD_COUNT" -gt 0 ]; then
  echo "::notice::Vars in .env.example that config.go does not read ($DEAD_COUNT):"
  echo "$DEAD" | while IFS= read -r line; do
    echo "  - $line"
  done
  echo ""
fi

if [ "$MODE" = "diff" ]; then
  exit 0
fi

if [ "$MISSING_COUNT" -gt 0 ]; then
  echo "::error::FAIL: $MISSING_COUNT env var(s) missing from .env.example. Add them before committing."
  exit 1
fi

echo "Parity check passed - all $CODE_COUNT config.go env vars are documented in .env.example."
exit 0
