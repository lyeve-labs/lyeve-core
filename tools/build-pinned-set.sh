#!/usr/bin/env bash
# build-pinned-set.sh - build cmd/lyeve from exactly the commits a release
# would ship, and name every repository that does not compile at its pin.
#
# The release workflow clones lyeve-libs and every plugin cmd/lyeve imports at
# the SHA PLUGIN_VERSIONS.txt pins, and builds them against this checkout. A
# local build reads whatever each sibling has checked out, so a pin that does
# not compile against the engine passes every local gate and fails only in
# the release job. This assembles the release's workspace from the pinned
# commits instead.
#
# Usage: build-pinned-set.sh [--at REF] [--repos DIR] [--out DIR]
#
#   --at REF     take every pinned repository at REF (for example origin/dev)
#                instead of its pin, to ask whether that ref would build
#   --repos DIR  where the sibling clones live (default: this repo's parent)
#   --out DIR    where to assemble the workspace (default: a new temp dir)
#
# Each repository is exported with git archive from the local clone, so the
# commit must have been fetched. This checkout joins the workspace with its
# working tree as it stands. The workspace is left in place, and its path is
# printed, so a follow-up such as `cd <dir>/lyeve-core && make lint-go-release`
# runs against the same set. Go reads the logical working directory, so the
# follow-up has to start from that path and not from this checkout.
#
# Exit status: 0 when cmd/lyeve and every pinned repository build, 1 when any
# fails, 2 on a usage or setup error.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPOS="$(cd "$ROOT/.." && pwd)"
LOCKFILE="$ROOT/PLUGIN_VERSIONS.txt"
AT=""
OUT=""

while [ $# -gt 0 ]; do
    case "$1" in
        --at) AT="$2"; shift 2 ;;
        --repos) REPOS="$(cd "$2" && pwd)"; shift 2 ;;
        --out) OUT="$2"; shift 2 ;;
        -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

if [ -z "$OUT" ]; then
    OUT="$(mktemp -d "${TMPDIR:-/tmp}/lyeve-pinned-set.XXXXXX")"
elif [ -e "$OUT" ] && [ -n "$(ls -A "$OUT" 2>/dev/null)" ]; then
    echo "ERROR: $OUT exists and is not empty" >&2
    exit 2
fi
mkdir -p "$OUT"

# The same discovery the release workflow uses: active blank imports only.
plugins=$(grep -oE '^[[:space:]]*_ "github\.com/lyeve-labs/lyeve-plugin-[a-z0-9-]+/plugin"' \
            "$ROOT/cmd/lyeve/main.go" | grep -oE 'lyeve-plugin-[a-z0-9-]+' | sort -u)
if [ -z "$plugins" ]; then
    echo "ERROR: no plugin imports found in cmd/lyeve/main.go" >&2
    exit 2
fi

setup_errors=0
for r in lyeve-libs $plugins; do
    if [ -n "$AT" ]; then
        ref="$AT"
    else
        ref=$(awk -v n="$r" '$1 == n { print $3 }' "$LOCKFILE")
        if [ -z "$ref" ]; then
            echo "SETUP: $r is not pinned in PLUGIN_VERSIONS.txt" >&2
            setup_errors=$((setup_errors + 1))
            continue
        fi
    fi
    if ! sha=$(git -C "$REPOS/$r" rev-parse --verify --quiet "$ref^{commit}"); then
        echo "SETUP: $r has no commit $ref locally (fetch it first)" >&2
        setup_errors=$((setup_errors + 1))
        continue
    fi
    mkdir -p "$OUT/$r"
    git -C "$REPOS/$r" archive "$sha" | tar -x -C "$OUT/$r"
    printf '%s %s\n' "$r" "$sha" >> "$OUT/SET.txt"
done
if [ "$setup_errors" -gt 0 ]; then
    echo "ERROR: $setup_errors repositories could not be exported, workspace at $OUT" >&2
    exit 2
fi

# Plugins replace the engine with a sibling path, so this checkout joins the
# workspace as lyeve-core beside them, through a link that keeps the working
# tree as it stands.
ln -s "$ROOT" "$OUT/lyeve-core"
(
    cd "$OUT"
    go work init
    # shellcheck disable=SC2086
    go work use ./lyeve-core ./lyeve-core/cmd/lyeve ./lyeve-libs $plugins
)

failed=()
for r in lyeve-libs $plugins; do
    if ! out=$(cd "$OUT" && go build "./$r/..." 2>&1); then
        failed+=("$r")
        printf '%s\n' "$out" > "$OUT/$r.build.log"
    fi
done
binary_ok=1
if ! (cd "$OUT/lyeve-core/cmd/lyeve" && go build -o "$OUT/lyeve" .) > "$OUT/cmd-lyeve.build.log" 2>&1; then
    binary_ok=0
fi

echo "workspace: $OUT"
echo "set: $OUT/SET.txt"
if [ ${#failed[@]} -gt 0 ]; then
    echo "FAIL: these repositories do not build at the commit taken:"
    for r in "${failed[@]}"; do
        printf '  %s @ %s\n' "$r" "$(awk -v n="$r" '$1 == n { print substr($2, 1, 12) }' "$OUT/SET.txt")"
        sed -n '1,6p' "$OUT/$r.build.log" | sed 's/^/      /'
    done
fi
if [ "$binary_ok" -eq 0 ]; then
    echo "FAIL: cmd/lyeve does not build, see $OUT/cmd-lyeve.build.log"
fi
if [ ${#failed[@]} -gt 0 ] || [ "$binary_ok" -eq 0 ]; then
    exit 1
fi
echo "OK: cmd/lyeve and $(wc -l < "$OUT/SET.txt") pinned repositories build"
