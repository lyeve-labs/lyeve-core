#!/usr/bin/env bash
# lint-mysql-text-default.sh - fail if a MySQL migration puts a literal
# DEFAULT on a TEXT, BLOB, JSON or GEOMETRY column.
#
# MySQL rejects the DDL outright:
#
#   ERROR 1101 (42000): BLOB, TEXT, GEOMETRY or JSON column 'reason'
#                       can't have a default value
#
# The same line is legal on PostgreSQL, so a migration passes in development
# and fails on the first MySQL install.
#
# Since 8.0.13 MySQL accepts a parenthesized default expression, which is how
# the rest of the codebase writes it:
#
#     `reason` LONGTEXT NOT NULL DEFAULT ('')
#
# Scans this repository, or the tree passed as the first argument.
#
# Run by CI on every PR. Called from Makefile `make verify`.
set -euo pipefail

ROOT="$(cd "${1:-$(dirname "$0")/..}" && pwd)"
VIOLATIONS=0

# Type names are matched as whole tokens. mawk has no \b, so the boundary is
# spelled out as "not an identifier character" on either side.
TYPES='TINYTEXT|TEXT|MEDIUMTEXT|LONGTEXT|TINYBLOB|BLOB|MEDIUMBLOB|LONGBLOB|JSON|GEOMETRY'

while IFS= read -r -d '' file; do
    matches=$(awk -v types="$TYPES" '
        { line = $0 }
        # Strip trailing comments so a type name in prose is not a match.
        { sub(/--.*$/, "", line) }
        # DEFAULT ( ... ) is the supported form. DEFAULT NULL is always legal.
        line ~ ("(^|[^A-Za-z_])(" types ")([^A-Za-z_(]|$)") &&
        line ~ /DEFAULT[[:space:]]+('"'"'|[0-9])/ {
            printf "%d:%s\n", NR, $0
        }
    ' "$file")

    if [ -n "$matches" ]; then
        relpath="${file#$ROOT/}"
        while IFS= read -r line; do
            echo "VIOLATION: ${relpath}:${line}"
        done <<< "$matches"
        VIOLATIONS=$((VIOLATIONS + $(echo "$matches" | wc -l)))
    fi
done < <(find "$ROOT" -maxdepth 3 -path '*/migrations/mysql/*.sql' -print0 2>/dev/null)

if [ "$VIOLATIONS" -gt 0 ]; then
    echo ""
    echo "ERROR: $VIOLATIONS literal DEFAULT(s) on a TEXT/BLOB/JSON column in MySQL DDL"
    echo "MySQL fails this DDL with error 1101 while PostgreSQL accepts it, so the"
    echo "migration passes in development and breaks on the first MySQL install."
    echo "Use a parenthesized expression instead: DEFAULT ('')"
    exit 1
fi

echo "OK: no literal DEFAULTs on TEXT/BLOB/JSON columns in MySQL migrations"
