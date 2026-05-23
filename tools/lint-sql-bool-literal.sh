#!/usr/bin/env bash
# lint-sql-bool-literal.sh - refuse a boolean literal inside SQL text.
#
# SQL Server has no TRUE or FALSE keyword. A BIT column compares against 1 and
# 0, so `WHERE enabled = true` is not a type mismatch there, it is
# `Invalid column name 'true'`: the parser reads the bare word as an
# identifier. PostgreSQL accepts the literal and MySQL accepts it as an alias
# for 1, so the statement passes review, passes local development, passes the
# PostgreSQL leg of CI, and fails only where SQL Server runs.
#
# What to write instead: bind the value.
#
#   rows, err := q.Query(ctx, `SELECT ... WHERE enabled = $1`, true)
#
# The driver converts a Go bool to whatever the column wants on each dialect,
# which is the whole point of the placeholder rewriter. Where a literal is
# genuinely required, for instance inside a partial index predicate in a
# migration, write it per dialect: PostgreSQL takes TRUE, SQL Server takes 1.
#
# Only non-test source is scanned. A test that runs a boolean literal against
# SQL Server fails on the spot and names itself, and the tests that carry this
# pattern are asserting the query text a store emits rather than emitting it.
# The bug this catches is the one with no test looking at it.
#
# A line that legitimately carries the pattern marks itself on the line above:
#
#   // lyeve:sql-bool-literal <reason>
set -euo pipefail
export LC_ALL=C

# The scan covers this repository, or the tree passed as the first argument.
ROOT="$(cd "${1:-$(dirname "$0")/..}" && pwd)"

if [ -z "$(find "$ROOT" -name '*.go' -not -path '*/.git/*' -print -quit)" ]; then
    echo "::error::no Go source under $ROOT; refusing to report a clean run"
    exit 1
fi

# The keyword prefix is what separates SQL from a Go expression: `enabled ==
# true` in Go has no WHERE, AND, OR, SET, HAVING or ON in front of it, and a
# comparison written in SQL always does.
RAW="$(grep -rn --include='*.go' -E \
        '(WHERE|AND|OR|SET|HAVING|ON)[[:space:]]+[a-zA-Z_."`[]+[]`"]?[[:space:]]*(=|<>|!=)[[:space:]]*(true|false|TRUE|FALSE)([^a-zA-Z0-9_]|$)' \
        "$ROOT" 2>/dev/null \
        | grep -v '_test\.go:' \
        | grep -vE ':[[:space:]]*(//|\*)' || true)"

HITS=""
while IFS= read -r hit; do
    [ -z "$hit" ] && continue
    file="${hit%%:*}"
    rest="${hit#*:}"
    line="${rest%%:*}"
    prev=$((line - 1))
    if [ "$prev" -ge 1 ] && sed -n "${prev}p" "$file" 2>/dev/null | grep -qE 'lyeve:sql-bool-literal[[:space:]]+[^[:space:]]'; then
        continue
    fi
    HITS="${HITS}${hit}"$'\n'
done <<< "$RAW"
HITS="$(printf '%s' "$HITS")"

if [ -n "$HITS" ]; then
    echo "::error::boolean literal in SQL text. SQL Server has no TRUE or FALSE"
    echo "keyword, so this reads as an unknown column name there and the statement"
    echo "fails at run time on that dialect alone:"
    echo ""
    echo "$HITS" | sed 's|'"$ROOT"'/||' | sed 's/^/  /'
    echo ""
    echo "Bind the value instead: WHERE enabled = \$1 with a Go bool argument."
    exit 1
fi

echo "OK: no boolean literal in SQL text."
