#!/usr/bin/env bash
# lint-no-raw-error-text.sh - fail if any production Go handler leaks raw error
# text (err.Error()) into an HTTP response via httpx.Error, httpx.ErrorReq, or
# http.Error.
#
# Handlers must return static human-readable messages.  DB driver error text,
# validation internals, and stack traces must never be echoed to API clients
# because they leak implementation details and can be exploited by attackers
# probing for SQL grammar, column names, or driver versions.
#
# Exclusions:
# - *_test.go - test assertions may exercise error paths directly
# - internal/middleware/ - middleware builds its messages from request fields,
#     not DB errors
# - comment lines - godoc examples describe the call, they are not it
# - //nolint:raw-error-text - for errors this codebase wrote itself, such as
#     reqparse and struct-validation failures.  Requires a justification after
#     the directive.
#
# Takes an optional root, and scans exactly that tree. Without one it scans
# this repository.
#
#   tools/lint-no-raw-error-text.sh [dir]
#
# Called from Makefile `make verify`.
set -euo pipefail

CORE="$(cd "$(dirname "$0")/.." && pwd)"

ROOT=""
for arg in "$@"; do
    case "$arg" in
        -*) echo "unknown flag: $arg" >&2; exit 2 ;;
        *) ROOT="$arg" ;;
    esac
done

ROOT="${ROOT:-$CORE}"
VIOLATIONS=0

while IFS= read -r -d '' file; do
    # Skip test and generated files.
    case "$file" in
        *_test.go|*mock*.go|*fake*.go|*/coverage.html)
            continue
            ;;
    esac

    # Middleware is allowed to construct error messages from request fields.
    case "$file" in
        "$ROOT/internal/middleware/"*)
            continue
            ;;
    esac

    # Three shapes. err.Error() is the obvious spelling. Formatting the error
    # into the message with %v or %s leaks it as surely as err.Error(), and so
    # does any helper taking w with err.Error() on the line, which is what a
    # plugin's own envelope looks like:
    # writeJSON(w, code, map[string]string{"error": err.Error()}).
    if matches=$(grep -nE 'httpx\.Error(Req)?\(w.*err\.Error\(\)|http\.Error\(w.*err\.Error\(\)|httpx\.Error(Req)?\(w.*fmt\.Sprintf\(.*[^a-zA-Z_]err[^a-zA-Z_.]|http\.Error\(w.*fmt\.Sprintf\(.*[^a-zA-Z_]err[^a-zA-Z_.]|[A-Za-z_][A-Za-z0-9_.]*\(w[,)].*err\.Error\(\)' "$file" 2>/dev/null \
        | grep -v '^[0-9]*:[[:space:]]*//' \
        | grep -v 'nolint:raw-error-text'); then
        relpath="${file#$ROOT/}"
        while IFS= read -r line; do
            echo "VIOLATION: ${relpath}:${line}"
        done <<< "$matches"
        VIOLATIONS=$((VIOLATIONS + $(echo "$matches" | wc -l)))
    fi
done < <(find "$ROOT" -name '.worktrees' -prune -o -name '*.go' -print0)

if [ "$VIOLATIONS" -gt 0 ]; then
    echo ""
    echo "ERROR: $VIOLATIONS raw error text leak(s) found in handler responses"
    echo "Handlers must return static human-readable messages - never err.Error()."
    exit 1
fi

echo "OK: no raw error text leaks in handler responses"
