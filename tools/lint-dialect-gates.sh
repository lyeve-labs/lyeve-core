#!/usr/bin/env bash
# lint-dialect-gates.sh: fail if a test starts a database server without
# first asking whether the run covers that server's dialect.
#
# CI_DIALECT limits a run to one dialect, and the CI matrix gives each dialect
# its own job. A PostgreSQL run that starts MySQL or SQL Server pays minutes and
# gigabytes for a server nobody asked for, and on a machine other runs share,
# that memory is what makes SQL Server abort mid-suite. testdb and plugintest
# ask on their callers' behalf. A test that starts its own server has to ask
# itself. The scan parses Go, so it is a small program in tools/dialectgates,
# whose doc comment lists the checks it accepts.
set -uo pipefail

# The tree to scan defaults to this repository. The tests pass a fixture.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="${1:-$ROOT}"
TARGET="$(cd "$TARGET" && pwd)" || exit 2
cd "$ROOT" && go run ./tools/dialectgates "$TARGET"
