#!/usr/bin/env bash
# verify-core-pure.sh - fail if any file under pkg/core (except
# enginehost/) imports github.com/lyeve-labs/lyeve-core/internal/*.
#
# pkg/core is the stable public API surface for plugins. Engine details live in
# pkg/core/enginehost/, which is allowed to import internal/* because it IS the
# engine. Everything else in pkg/core/ must be pure-interface (types,
# contracts, helpers) with zero internal coupling.
#
# Go's own internal rule cannot enforce this: pkg/core and internal/ sit in the
# same module, so the compiler permits the import. This script is the only
# guard, which is why it aborts rather than passing when it finds nothing to
# scan.
#
# Run by CI on every PR. Called from Makefile `make verify`.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="$ROOT/pkg/core"
IMPORT_PREFIX='github.com/lyeve-labs/lyeve-core/internal/'
SCAN="$ROOT/tools/internal-import-scan.awk"
VIOLATIONS=0
SCANNED=0

if [ ! -d "$TARGET" ]; then
    echo "ERROR: $TARGET does not exist - this guard cannot run."
    echo "If pkg/core moved, update TARGET here rather than deleting the check."
    exit 1
fi

while IFS= read -r -d '' file; do
    # enginehost/ implements the Host, so internal/* is allowed there.
    if [[ "$file" == */pkg/core/enginehost/* ]]; then
        continue
    fi
    SCANNED=$((SCANNED + 1))
    if matches=$(awk -v prefix="$IMPORT_PREFIX" -f "$SCAN" "$file"); [ -n "$matches" ]; then
        while IFS= read -r line; do
            echo "VIOLATION: ${line#"$ROOT"/}"
        done <<< "$matches"
        VIOLATIONS=$((VIOLATIONS + $(echo "$matches" | wc -l)))
    fi
done < <(find "$TARGET" -name '.worktrees' -prune -o -name '*.go' -print0)

if [ "$SCANNED" -eq 0 ]; then
    echo "ERROR: scanned 0 files under $TARGET - the guard is not doing anything."
    exit 1
fi

if [ "$VIOLATIONS" -gt 0 ]; then
    echo ""
    echo "ERROR: $VIOLATIONS internal/* import site(s) found in pkg/core/"
    echo "pkg/core must be a pure-interface package - no imports of internal/*."
    echo "Move implementation code into pkg/core/enginehost/ if it needs internal/*."
    exit 1
fi

echo "OK: pkg/core is pure - $SCANNED files, zero internal/* imports (outside enginehost/)."
