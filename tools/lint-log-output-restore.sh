#!/usr/bin/env bash
# lint-log-output-restore.sh: fail if a test makes a logger the default and
# never puts back the log package's output.
#
# slog.SetDefault also points the log package at the new logger's handler,
# and making the old logger the default again does not undo it. Every later
# default log line in the test binary then lands in the handler of a test that
# has ended, so a later failure prints none of its own log lines.
#
# The scan parses Go, so it is a small program beside this script. Its doc
# comment says what counts as putting the output back and how a function that
# keeps it on purpose marks itself:
#
#   // lyeve:log-output-kept <reason>
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="${1:-$ROOT}"
TARGET="$(cd "$TARGET" && pwd)" || exit 2
cd "$ROOT" && go run ./tools/logrestore "$TARGET"
