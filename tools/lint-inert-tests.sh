#!/usr/bin/env bash
# lint-inert-tests.sh - fail if a Go test function contains no assertion.
#
# A test that only calls t.Log executes, prints prose and passes whatever the
# code does. That is worse than no test: a green test named for a case states
# the case has been proved, so the gap is invisible to exactly the reviewer who
# would otherwise close it.
#
# A function counts as asserting when it calls t.Error/t.Fatal (or the f forms),
# uses testify, skips, or hands t to any helper. That last rule is deliberately
# generous: a helper taking t is assumed to assert, so the scan under-reports
# rather than flagging a table-driven test whose checks live one call away.
#
# Some tests prove something real without an assertion: one that fails by
# panicking, and one that exists for the race detector. Those declare it with a
# marker on the function, which the scan honors:
#
#   // lyeve:no-assert fails by panicking. There is nothing to compare
#   func TestUnsubscribeTwiceDoesNotPanic(t *testing.T) {
#
# The known set is frozen in tools/inert-test-baseline.txt. A new one fails. An
# entry that stops reproducing fails too, so the file can only shrink.
#
#   tools/lint-inert-tests.sh                        # check
#   tools/lint-inert-tests.sh '' baseline            # rewrite the baseline
set -euo pipefail
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# The scope is this repository, or the tree passed as the first argument.
TREE="${1:-}"
TREE="${TREE:-$ROOT}"
MODE="${2:-check}"
BASELINE="${LYEVE_INERT_TEST_BASELINE:-$ROOT/tools/inert-test-baseline.txt}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

python3 - "$TREE" > "$TMP/found.tsv" <<'PY'
import os, re, sys

root = sys.argv[1]

# Anything that can fail the test, plus a skip, plus handing t to a helper.
ASSERTS = re.compile(
    r't\.(Error|Fatal|Errorf|Fatalf|Skip|Skipf|SkipNow)\b'
    r'|require\.|assert\.'
    r'|\w+\(\s*t\s*[,)]'
    r'|\(\s*t\s*,'
)
FUNC = re.compile(r'\nfunc (Test\w+)\s*\(')

# A marker on the doc comment exempts the function below it. It has to carry a
# reason: "// lyeve:no-assert" alone says only that someone wanted it to pass.
MARKER = re.compile(r'//\s*lyeve:no-assert\s+\S')

# The file column is relative to the scanned root, so a clone into any folder
# name reports the same entries and the baseline matches either way.
for dirpath, dirs, names in os.walk(root):
    dirs[:] = [d for d in dirs if d not in ('.git', '.worktrees', 'vendor', 'node_modules', 'testdata')]
    for n in names:
        if not n.endswith('_test.go'):
            continue
        full = os.path.join(dirpath, n)
        rel = os.path.relpath(full, root)
        try:
            src = open(full, encoding='utf-8', errors='replace').read()
        except OSError:
            continue
        parts = FUNC.split('\n' + src)
        for i in range(1, len(parts), 2):
            name_, body = parts[i], parts[i + 1]
            if ASSERTS.search(body):
                continue
            # The marker sits in the comment block above the function, which
            # is the tail of the preceding chunk.
            if MARKER.search(parts[i - 1][-400:]):
                continue
            print(f"{name_}\t{rel}")
PY

sort -o "$TMP/found.tsv" "$TMP/found.tsv"

# An empty scan and a clean scan produce the same output, so the run has to
# prove it read something, by finding the repository's own Go test files.
# -quit rather than head: a closed pipe under pipefail exits 141, which reads
# as a failing lint rather than as a scan that found its first file.
SCANNED="$(find "$TREE" -name '*_test.go' -not -path '*/.git/*' -print -quit | wc -l | tr -d ' ')"
if [ "$SCANNED" -eq 0 ]; then
    echo "::error::no Go test files under $TREE, refusing to report a clean run"
    exit 1
fi

if [ "$MODE" = "baseline" ]; then
    {
        echo "# Test functions in this repository that contain no assertion."
        echo "# Format: test<TAB>file, the file relative to the repository root."
        echo "#"
        echo "# The file can only shrink: a new one fails the lint and an entry that"
        echo "# stops reproducing fails it too, so a fix has to delete its line."
        echo "#"
        echo "# Regenerate with: tools/lint-inert-tests.sh '' baseline"
        cat "$TMP/found.tsv"
    } > "$BASELINE"
    echo "OK: baseline rewritten with $(grep -cv '^#' "$BASELINE") entr(ies)."
    exit 0
fi

if [ ! -f "$BASELINE" ]; then
    echo "::error::baseline missing at $BASELINE, refusing to report a clean run"
    exit 1
fi

grep -v '^#' "$BASELINE" | grep -v '^[[:space:]]*$' | sort > "$TMP/baseline.tsv" || true

comm -23 "$TMP/found.tsv" "$TMP/baseline.tsv" > "$TMP/new.tsv" || true
comm -13 "$TMP/found.tsv" "$TMP/baseline.tsv" > "$TMP/stale.tsv" || true

NEW="$(wc -l < "$TMP/new.tsv" | tr -d ' ')"
STALE="$(wc -l < "$TMP/stale.tsv" | tr -d ' ')"
TOTAL="$(wc -l < "$TMP/baseline.tsv" | tr -d ' ')"

fail=0

if [ "$NEW" -gt 0 ]; then
    echo "::error::$NEW test function(s) assert nothing:"
    while IFS=$'\t' read -r test file; do
        echo "  $test  ($file)"
    done < "$TMP/new.tsv"
    echo
    echo "A test that only logs passes whatever the code does. Assert the claim in"
    echo "its name, or delete it. If the checks live in a helper, pass t to that"
    echo "helper and this lint will see it."
    echo
    echo "A test that proves something without asserting (one that fails by"
    echo "panicking, or one that exists for the race detector) says so:"
    echo
    echo "    // lyeve:no-assert fails by panicking. There is nothing to compare"
    fail=1
fi

if [ "$STALE" -gt 0 ]; then
    echo "::error::$STALE baseline entr(ies) do not reproduce. Delete them from"
    echo "tools/inert-test-baseline.txt so the file keeps shrinking:"
    while IFS=$'\t' read -r test file; do
        echo "  $test  ($file)"
    done < "$TMP/stale.tsv"
    fail=1
fi

[ "$fail" -eq 0 ] || exit 1

echo "OK: $TOTAL known inert test(s) in the baseline, 0 new."
