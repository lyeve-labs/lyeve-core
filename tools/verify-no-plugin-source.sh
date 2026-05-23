#!/usr/bin/env bash
# verify-no-plugin-source.sh - Fail if engine source imports a plugin module.
#
# Plugins are compiled only by cmd/lyeve, a module of its own, so the engine
# module builds and runs with none of them.
#
# Run by CI on every PR. Called from Makefile `make verify`.
#
# NOTE: this script must be executable (chmod +x). The Makefile target invokes
# it via `bash` to be safe in case the +x bit is stripped by tooling.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VIOLATIONS=0

# The skip of cmd/lyeve below is only sound while cmd/lyeve is a module of its
# own: that is what keeps its plugin imports out of the engine module.
if [ ! -f "$ROOT/cmd/lyeve/go.mod" ]; then
  echo "ERROR: cmd/lyeve/go.mod is missing, so cmd/lyeve is part of the" >&2
  echo "engine module and its plugin imports DO reach the engine build." >&2
  exit 1
fi

PATTERNS=(
  'github\.com/lyeve-labs/lyeve-plugin-'  # every plugin module lives under this prefix
)

while IFS= read -r -d '' file; do
  # Skip this file itself (false positive)
  if [[ "$file" == *"verify-no-plugin-source.sh" ]]; then
    continue
  fi
  # cmd/lyeve is the module that compiles the plugins in, so it imports them.
  # The engine module never compiles it, because it has its own go.mod.
  if [[ "$file" == *"/cmd/lyeve/"* ]]; then
    continue
  fi
  for pattern in "${PATTERNS[@]}"; do
    if grep -qE "$pattern" "$file" 2>/dev/null; then
      echo "VIOLATION: $file references a plugin module ($pattern)"
      VIOLATIONS=$((VIOLATIONS + 1))
    fi
  done
done < <(find "$ROOT/cmd" "$ROOT/internal" "$ROOT/pkg" "$ROOT/migrations" \
             "$ROOT/tests" "$ROOT/scripts" "$ROOT/tools" \
             -type f \( -name '*.go' -o -name 'Dockerfile' -o -name '*.yaml' -o -name '*.yml' -o -name '*.toml' -o -name 'Makefile' \) -print0 2>/dev/null)

if [ "$VIOLATIONS" -gt 0 ]; then
  echo ""
  echo "ERROR: $VIOLATIONS file(s) reference a plugin module from inside lyeve-core."
  echo "The engine module MUST NOT import or reference a plugin module."
  echo "Plugins are compiled only by cmd/lyeve, a Go module of its own."
  exit 1
fi

echo "OK: the engine module references no plugin module."
