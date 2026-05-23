#!/usr/bin/env bash
# lock-plugin-versions.sh - rewrite PLUGIN_VERSIONS.txt from the pushed state
# of every repo the release build clones.
#
# Pins come from each repo's default branch on origin, never the local HEAD.
# The release workflow clones these SHAs from GitHub, so a SHA that exists only
# in a local checkout fails the build at clone time, long after the lockfile was
# committed and reviewed.
#
# Local commits ahead of origin are reported, not pinned: unreviewed work does
# not belong in a release, and staying silent about it would hide the fact that
# a fix believed to be shipping is not.
#
# Remote-tracking refs are read as they stand. Run `git fetch` in the repos you
# care about first if the lockfile needs to reflect a very recent push.
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
WORKSPACE="$(cd "$ROOT/.." && pwd)"
LOCKFILE="$ROOT/PLUGIN_VERSIONS.txt"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

# defaultRemoteRef prints the remote-tracking ref holding a repo's reviewed
# state, or nothing when the repo has no usable remote branch.
defaultRemoteRef() {
    local dir=$1 ref
    ref=$(git -C "$dir" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)
    if [ -n "$ref" ] && git -C "$dir" rev-parse --verify --quiet "$ref" >/dev/null 2>&1; then
        printf '%s' "$ref"
        return
    fi
    for candidate in origin/main origin/master; do
        if git -C "$dir" rev-parse --verify --quiet "$candidate" >/dev/null 2>&1; then
            printf '%s' "$candidate"
            return
        fi
    done
}

{
    printf '%s\n' \
        '# Release pin file. Pins every repository the release build clones beside' \
        '# this one to an exact commit SHA, so builds are reproducible and a' \
        '# supply-chain attack has to modify this file, which lives here under code' \
        '# review.' \
        '#' \
        '# Format:  repo-name = full-40-char-commit-sha' \
        '# Empty lines and lines starting with # are ignored.' \
        '#' \
        '# Covers the license module cmd/lyeve replaces with a sibling path, and every' \
        '# plugin cmd/lyeve compiles in. release.yml refuses to build a repository' \
        '# missing from this file.' \
        '#' \
        '# SHAs are the tip of each repository'\''s default branch on origin, never a' \
        '# local HEAD: the release workflow clones them from GitHub, so an unpushed' \
        '# commit fails the build. Run `make lock-plugin-versions` to update, and' \
        '# review the diff before committing.' \
        '#' \
        "# Generated $(date -u +%Y-%m-%dT%H:%M:%SZ) from origin default branches."\
        ''
} > "$TMP"

missing=()
unpushed=()
pinned=0

for dir in "$WORKSPACE"/lyeve-libs "$WORKSPACE"/lyeve-plugin-*; do
    [ -d "$dir/.git" ] || continue
    name=$(basename "$dir")

    ref=$(defaultRemoteRef "$dir")
    if [ -z "$ref" ]; then
        missing+=("$name")
        continue
    fi

    sha=$(git -C "$dir" rev-parse "$ref")
    echo "$name = $sha" >> "$TMP"
    pinned=$((pinned + 1))

    ahead=$(git -C "$dir" rev-list --count "$ref"..HEAD 2>/dev/null || echo 0)
    if [ "$ahead" != "0" ]; then
        branch=$(git -C "$dir" rev-parse --abbrev-ref HEAD)
        unpushed+=("$name ($branch, $ahead commit(s) ahead of $ref)")
    fi
done

if [ ${#missing[@]} -gt 0 ]; then
    echo "ERROR: no origin default branch to pin for:" >&2
    printf '  %s\n' "${missing[@]}" >&2
    echo "" >&2
    echo "Push the repo and fetch, or remove it from the workspace. The lockfile was" >&2
    echo "left unchanged rather than written with a gap the release build would hit." >&2
    exit 1
fi

mv "$TMP" "$LOCKFILE"

echo "OK: PLUGIN_VERSIONS.txt updated ($pinned repos pinned from origin)"

if [ ${#unpushed[@]} -gt 0 ]; then
    echo ""
    echo "NOT pinned, local commits are not on origin:"
    printf '  %s\n' "${unpushed[@]}"
    echo ""
    echo "A release built from this lockfile will not contain that work."
fi
