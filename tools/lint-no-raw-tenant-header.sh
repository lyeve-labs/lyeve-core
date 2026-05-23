#!/usr/bin/env bash
# lint-no-raw-tenant-header.sh - fail if any production Go file reads the
# X-Tenant-ID header directly (r.Header.Get("X-Tenant-ID")) instead of going
# through core.TenantIDFromCtx(ctx).
#
# The tenant middleware injects the resolved tenant ID into the request
# context.  Handler code must read it from there. Reading the raw header
# bypasses the middleware's auth checks and is a multi-tenancy isolation
# vulnerability.
#
# Exclusions:
# - internal/middleware/ - the middleware itself implements the header read
# - internal/api/middleware.go - CORS allowlist comment only
# - *_test.go - tests may exercise middleware boundaries
# - coverage.html - generated, not source
#
# Run by CI on every PR. Called from Makefile `make verify`.
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
VIOLATIONS=0

while IFS= read -r -d '' file; do
    # Skip generated and test files.
    case "$file" in
        */coverage.html|*_test.go|*mock*.go|*fake*.go)
            continue
            ;;
    esac

    # Middleware is allowed to read X-Tenant-ID - it's implementing the mechanism.
    case "$file" in
        "$ROOT/internal/middleware/"*|"$ROOT/internal/api/middleware.go")
            continue
            ;;
    esac

    # A read is suppressed by //nolint:raw-tenant-header on the same line or the
    # line above, and the marker must carry a justification. Public
    # unauthenticated routes resolve their own tenant because the middleware has
    # not run yet, and there is no context value for them to read.
    matches=$(awk '
        /\/\/nolint:raw-tenant-header[[:space:]]+[^[:space:]]/ {
            suppressed[NR] = 1
            suppressed[NR + 1] = 1
        }
        /Header\.Get\("X-Tenant-ID"\)/ {
            if (!suppressed[NR]) printf "%d:%s\n", NR, $0
        }
    ' "$file")
    if [ -n "$matches" ]; then
        relpath="${file#$ROOT/}"
        while IFS= read -r line; do
            echo "VIOLATION: ${relpath}:${line}"
        done <<< "$matches"
        VIOLATIONS=$((VIOLATIONS + $(echo "$matches" | wc -l)))
    fi
# Skip nested git worktrees, which hold other checkouts.
done < <(find "$ROOT" -name '.worktrees' -prune -o -name '*.go' -print0)

if [ "$VIOLATIONS" -gt 0 ]; then
    echo ""
    echo "ERROR: $VIOLATIONS direct X-Tenant-ID header read(s) found outside middleware/"
    echo "Handlers must use core.TenantIDFromCtx(ctx) - never r.Header.Get(\"X-Tenant-ID\")."
    exit 1
fi

echo "OK: no raw X-Tenant-ID header reads outside middleware/"
