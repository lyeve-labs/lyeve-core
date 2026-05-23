#!/usr/bin/env bash
# lint-mysql-inline-references.sh - fail if a MySQL migration declares a
# foreign key on the column rather than on the table.
#
# InnoDB parses a REFERENCES clause written inside a column definition and
# discards it. No error, no warning, no key:
#
#     uploaded_by  CHAR(36)  REFERENCES sys_users(id) ON DELETE SET NULL
#
# The same line creates a real key on PostgreSQL and SQL Server, so the three
# migration trees agree word for word and only one of them has the constraint.
# Whatever the key was to do on delete then happens on two engines and not the
# third, silently, for as long as nobody reads the catalog.
#
# The form InnoDB reads is a table-level clause, which every dialect accepts:
#
#     FOREIGN KEY (uploaded_by) REFERENCES sys_users(id) ON DELETE SET NULL
#
# Only the MySQL tree is read. The other two dialects honor both spellings, so
# a column-level clause there is a style question rather than a defect.
#
# Scans this repository, or the tree passed as the first argument.
#
# Run by CI on every PR. Called from Makefile `make verify`.
set -euo pipefail

ROOT="$(cd "${1:-$(dirname "$0")/..}" && pwd)"
VIOLATIONS=0
SCANNED=0

while IFS= read -r -d '' file; do
    SCANNED=$((SCANNED + 1))
    matches=$(awk '
        { line = $0 }
        # Comments first, so a REFERENCES in prose is not a match.
        { sub(/--.*$/, "", line) }
        # Upper-case the line so the match does not depend on how it was typed.
        { u = toupper(line) }
        u !~ /REFERENCES/ { next }
        # A table-level clause opens with FOREIGN KEY, or with a named
        # CONSTRAINT, or continues one that opened on an earlier line. ALTER
        # and the string form used inside a PREPARE are table-level too.
        u ~ /^[[:space:]]*(FOREIGN[[:space:]]+KEY|CONSTRAINT|ADD|ALTER)/ { next }
        u ~ /^[[:space:]]*'"'"'?[[:space:]]*REFERENCES/ { next }
        u ~ /FOREIGN[[:space:]]+KEY/ { next }
        { printf "%d:%s\n", NR, $0 }
    ' "$file")

    if [ -n "$matches" ]; then
        relpath="${file#$ROOT/}"
        while IFS= read -r line; do
            echo "VIOLATION: ${relpath}:${line}"
        done <<< "$matches"
        VIOLATIONS=$((VIOLATIONS + $(echo "$matches" | wc -l)))
    fi
done < <(find "$ROOT" -maxdepth 3 -path '*/migrations/mysql/*.sql' -print0 2>/dev/null)

# Scanning nothing is not a pass. An empty or mis-rooted checkout must not read
# as a clean one.
if [ "$SCANNED" -eq 0 ]; then
    echo "::error::no MySQL migrations found under $ROOT"
    echo "Pass the tree to scan: tools/lint-mysql-inline-references.sh /path/to/tree"
    exit 1
fi

if [ "$VIOLATIONS" -gt 0 ]; then
    echo ""
    echo "ERROR: $VIOLATIONS foreign key(s) declared on the column in MySQL DDL"
    echo "InnoDB parses a column-level REFERENCES and discards it, so the column is"
    echo "created and the key is not. PostgreSQL and SQL Server create it from the"
    echo "same line, so the three trees agree and only one engine lacks the key."
    echo "Write it as a table constraint instead:"
    echo "  FOREIGN KEY (col) REFERENCES other(id) ON DELETE SET NULL"
    exit 1
fi

echo "OK: $SCANNED MySQL migration file(s), every foreign key declared on the table"
