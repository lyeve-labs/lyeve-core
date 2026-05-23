#!/usr/bin/env bash
# lint-no-insert-ignore.sh - refuse INSERT IGNORE in Go source.
#
# MySQL's IGNORE modifier downgrades every error the statement can raise to a
# warning, not only the duplicate key it is usually reached for: a foreign key
# with no parent row, a value too long for its column, a NULL in a NOT NULL
# column, a lock-wait timeout, a deadlock. All of them insert nothing and report
# zero affected rows.
#
# That matters because zero affected rows is what callers read as "the row was
# already there", so a refused insert reads as an existing row and a rejected
# write is reported as a success. The plugin migrator, for one, would read it
# as "another boot already applied this version", skip the migration, and
# report a successful start.
#
# PostgreSQL and SQL Server have no equivalent. ON CONFLICT DO NOTHING and
# MERGE ... WHEN NOT MATCHED suppress the conflict and still raise everything
# else, which is why only the MySQL branch can be wrong this way.
#
# What to write instead:
#
#   - sqldialect.Upsert(dialect, UpsertConfig{..., DoNothing: true}) when the
#     row count is read. On MySQL it emits a guarded INSERT ... SELECT, which
#     counts rows actually inserted. Wrap it in sqlx.RetryOnTxConflict: the
#     probe takes a gap lock, so concurrent inserts into the same gap can
#     deadlock where IGNORE would not.
#   - INSERT ... ON DUPLICATE KEY UPDATE <pk> = <pk> when the row count is
#     discarded. It swallows a duplicate key and nothing else.
#
# SQL inside migration files is not scanned: a migration that seeds reference
# rows has no caller to mislead.
#
# Two situations are not this bug and mark themselves on the line above:
#
#   // lyeve:insert-ignore <reason>
#
# The first is code that recognizes the statement rather than emitting it - a
# SQL-dump parser matching a prefix. The second is a bulk import rewriting
# arbitrary user SQL, where the primary key is not known and so ON DUPLICATE
# KEY UPDATE cannot be written. The marker is not a general suppression: a
# statement this engine composes itself never needs it, because the column it
# would conflict on is right there in the code.
set -euo pipefail
export LC_ALL=C

# The scan covers this repository, or the tree passed as the first argument.
ROOT="$(cd "${1:-$(dirname "$0")/..}" && pwd)"

if [ -z "$(find "$ROOT" -name '*.go' -not -path '*/.git/*' -print -quit)" ]; then
    echo "::error::no Go source under $ROOT; refusing to report a clean run"
    exit 1
fi

# A comment explaining why the pattern is wrong is not a use of it. Only lines
# carrying the statement itself count, so the guard cannot be tripped by the
# prose that documents it.
RAW="$(grep -rn --include='*.go' -iE 'INSERT[[:space:]]+IGNORE[[:space:]]+INTO' \
        "$ROOT" 2>/dev/null \
        | grep -vE ':[[:space:]]*(//|\*)' || true)"

# Drop the lines whose immediately preceding line carries the marker.
HITS=""
while IFS= read -r hit; do
    [ -z "$hit" ] && continue
    file="${hit%%:*}"
    rest="${hit#*:}"
    line="${rest%%:*}"
    prev=$((line - 1))
    if [ "$prev" -ge 1 ] && sed -n "${prev}p" "$file" 2>/dev/null | grep -qE 'lyeve:insert-ignore[[:space:]]+[^[:space:]]'; then
        continue
    fi
    HITS="${HITS}${hit}"$'\n'
done <<< "$RAW"
HITS="$(printf '%s' "$HITS")"

if [ -n "$HITS" ]; then
    echo "::error::INSERT IGNORE found. It downgrades every error to a warning, so a"
    echo "rejected write reports zero affected rows and reads as an existing row:"
    echo ""
    echo "$HITS" | sed 's|'"$ROOT"'/||' | sed 's/^/  /'
    echo ""
    echo "Use sqldialect.Upsert(..., DoNothing: true) when the row count is read,"
    echo "or ON DUPLICATE KEY UPDATE <pk> = <pk> when it is discarded."
    exit 1
fi

echo "OK: no INSERT IGNORE in Go source."
