#!/usr/bin/env bash
# Every MSSQL MERGE used as an upsert must take a range lock.
#
# Without WITH (HOLDLOCK) a MERGE is not atomic against a concurrent MERGE on
# the same key: both can take the NOT MATCHED branch, and the loser fails the
# unique constraint instead of folding into the existing row. It shows up as a
# duplicate-key error under load and nowhere else, so it survives every
# single-threaded test.
#
# The target may be written bare, bracket-quoted or backtick-quoted, and the
# alias is optional. Comment lines are skipped: prose about MERGE is not a
# MERGE, and reporting it trains people to ignore the check.
#
# Usage: lint-mssql-merge-holdlock.sh [dir]
set -uo pipefail
root="${1:-.}"
found=0

ident='(\[[^]]+\]|`[^`]+`|"[^"]+"|[A-Za-z_][A-Za-z0-9_.]*)'

while IFS= read -r hit; do
  printf 'VIOLATION: %s\n' "$hit"
  found=$((found + 1))
done < <(
  grep -rnE "MERGE([[:space:]]+INTO)?[[:space:]]+${ident}" \
    --include='*.go' --include='*.sql' "$root" 2>/dev/null \
    | grep -v '_test\.go:' \
    | grep -iv 'HOLDLOCK' \
    | grep -vE ':[0-9]+:[[:space:]]*(//|--|\*|#)' \
    | awk -F: '{ body = $0; sub(/^[^:]*:[0-9]+:/, "", body);
                 m = index(body, "MERGE");
                 c = index(body, "//"); if (c > 0 && c < m) next;
                 d = index(body, "--"); if (d > 0 && d < m) next;
                 print }'
)

if [ "$found" -gt 0 ]; then
  echo "ERROR: $found MERGE statement(s) without WITH (HOLDLOCK)"
  exit 1
fi
echo "OK: every MERGE takes a range lock"
