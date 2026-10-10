#!/usr/bin/env bash
# lint-bare-goroutines.sh - fail if engine source starts a goroutine with no
# recovery.
#
# A panic in a goroutine ends the process, and nothing above it can catch it.
# The engine starts one with engine.GoSafe, which recovers and logs. The scan
# parses Go, so it is a small program in tools/baregoroutines. Its doc comment
# says how a go statement that has to stay marks itself:
#
#   // lyeve:bare-goroutine <reason>
set -uo pipefail

# The tree to scan defaults to this repository. The tests pass a fixture.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="${1:-$ROOT}"
TARGET="$(cd "$TARGET" && pwd)" || exit 2
cd "$ROOT" && go run ./tools/baregoroutines "$TARGET"
