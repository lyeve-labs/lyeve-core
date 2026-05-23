#!/usr/bin/env bash
# verify-plugin-versions.sh - fail if PLUGIN_VERSIONS.txt pins a commit the
# release build cannot clone.
#
# The release workflow clones each pinned SHA from GitHub. A SHA that exists
# only in a local checkout passes review, passes the build here, and fails in CI
# at clone time. This is the pre-flight that catches it locally.
#
# A pin is good when the commit is an ancestor of a remote-tracking ref, which
# is the same thing as saying it has been pushed. Remote-tracking refs are read
# as they stand, so fetch first if a pin was pushed very recently.
#
# Run before tagging a release. Called from Makefile `make verify-plugin-versions`.
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
WORKSPACE="$(cd "$ROOT/.." && pwd)"
LOCKFILE="$ROOT/PLUGIN_VERSIONS.txt"

if [ ! -f "$LOCKFILE" ]; then
    echo "ERROR: $LOCKFILE not found - run 'make lock-plugin-versions'" >&2
    exit 1
fi

problems=0
checked=0

while read -r name _eq sha; do
    case "$name" in ''|'#'*) continue ;; esac
    [ -n "${sha:-}" ] || continue

    dir="$WORKSPACE/$name"
    if [ ! -d "$dir/.git" ]; then
        echo "VIOLATION: $name is pinned but not checked out at $dir"
        problems=$((problems + 1))
        continue
    fi

    if ! git -C "$dir" cat-file -e "$sha^{commit}" 2>/dev/null; then
        echo "VIOLATION: $name pins $sha, which is not a commit in that repo"
        problems=$((problems + 1))
        continue
    fi

    if [ -z "$(git -C "$dir" branch -r --contains "$sha" 2>/dev/null)" ]; then
        echo "VIOLATION: $name pins $sha, which is on no remote branch (unpushed)"
        problems=$((problems + 1))
        continue
    fi

    checked=$((checked + 1))
done < <(sed 's/#.*//' "$LOCKFILE")

# Every plugin cmd/lyeve blank-imports has to be pinned, or the release exits
# before it compiles anything.
#
# The same discovery pattern as the workflow, so the two cannot disagree about
# what counts as an active import. A commented-out import does not match.
MAIN_GO="$(cd "$(dirname "$0")/.." && pwd)/cmd/lyeve/main.go"
imported=0
if [ -f "$MAIN_GO" ]; then
    while read -r name; do
        [ -n "$name" ] || continue
        imported=$((imported + 1))
        if ! awk -v n="$name" '$1 == n { found = 1 } END { exit !found }' "$LOCKFILE"; then
            echo "VIOLATION: $name is blank-imported by cmd/lyeve but not pinned"
            problems=$((problems + 1))
        fi
    done < <(grep -oE '^[[:space:]]*_ "github\.com/lyeve-labs/lyeve-plugin-[a-z0-9-]+/plugin"' "$MAIN_GO" \
             | grep -oE 'lyeve-plugin-[a-z0-9-]+' | sort -u)

    if [ "$imported" -eq 0 ]; then
        echo "VIOLATION: no plugin imports found in cmd/lyeve/main.go; the discovery pattern is stale"
        problems=$((problems + 1))
    fi
fi

if [ "$problems" -gt 0 ]; then
    echo ""
    echo "ERROR: $problems pin problem(s) in PLUGIN_VERSIONS.txt"
    echo "The release workflow clones these SHAs from GitHub and refuses to build"
    echo "without a pin for every imported plugin."
    echo "Push the missing commits and re-run 'make lock-plugin-versions'."
    exit 1
fi

echo "OK: all $checked pinned commits are present on a remote, and all $imported imported plugin(s) are pinned"
