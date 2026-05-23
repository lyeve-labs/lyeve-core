#!/usr/bin/env bash
# lint-no-raw-tx-placeholder.sh - fail if a statement run on a raw *sql.Tx
# hard-codes a $N placeholder.
#
# DB.Begin returns a plain *sql.Tx. Unlike the pool's Querier it carries no
# placeholder rewrite, so a literal $1 reaches MySQL and MSSQL verbatim and the
# statement fails there while passing on Postgres.
#
# Render placeholders per dialect instead, e.g.:
#
#     p := h.engine.Placeholder
#     tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM t WHERE id = %s", p(1)), id)
#
# A statement that is deliberately Postgres-only (pg_advisory_xact_lock, a
# `case "postgres":` branch) is legitimate: mark it //nolint:raw-tx-placeholder
# with the reason.
#
# Run by CI on every PR. Called from Makefile `make verify`.
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
VIOLATIONS=0

while IFS= read -r -d '' file; do
    case "$file" in
        */coverage.html|*_test.go|*mock*.go|*fake*.go)
            continue
            ;;
    esac

    matches=$(awk '
        # Track a dialect switch so per-engine branches are not flagged: the
        # Postgres arm of one is supposed to contain $N.
        /case "postgres"|case "postgresql"/ { pgcase = NR }
        /\/\/nolint:raw-tx-placeholder/ { suppressed[NR] = 1; suppressed[NR+1] = 1; suppressed[NR+2] = 1 }
        # A call back through the pool rewrites placeholders, so it ends the
        # window a nearby tx call opened rather than inheriting it.
        /pool\.(Exec|Query|QueryRow)|Querier\(/ { txcall = 0 }
        /tx\.(Exec|Query|QueryRow)Context\(/ { txcall = NR }
        {
            # A $N within three lines of a tx call, outside a postgres-only
            # branch, is the pattern that breaks on MySQL and MSSQL.
            if (txcall && NR - txcall <= 3 && /\$[0-9]/) {
                if (!suppressed[NR] && !(pgcase && NR - pgcase <= 8)) {
                    printf "%d:%s\n", NR, $0
                    txcall = 0
                }
            }
        }
    ' "$file")

    if [ -n "$matches" ]; then
        relpath="${file#$ROOT/}"
        while IFS= read -r line; do
            echo "VIOLATION: ${relpath}:${line}"
        done <<< "$matches"
        VIOLATIONS=$((VIOLATIONS + $(echo "$matches" | wc -l)))
    fi
done < <(find "$ROOT" -name '.worktrees' -prune -o -name '*.go' -print0)

if [ "$VIOLATIONS" -gt 0 ]; then
    echo ""
    echo "ERROR: $VIOLATIONS hard-coded \$N placeholder(s) on a raw *sql.Tx"
    echo "A raw *sql.Tx has no placeholder rewrite: \$1 fails on MySQL and MSSQL."
    echo "Render per dialect, or mark a deliberately Postgres-only statement"
    echo "//nolint:raw-tx-placeholder with the reason."
    exit 1
fi

echo "OK: no hard-coded \$N placeholders on raw transactions"
