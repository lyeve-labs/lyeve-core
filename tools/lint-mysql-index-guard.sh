#!/usr/bin/env bash
# lint-mysql-index-guard.sh - fail if a MySQL up migration creates an index
# with a statement of its own.
#
# MySQL 8 has no CREATE INDEX IF NOT EXISTS, so this pair is guarded on the
# table and bare on the index:
#
#     CREATE TABLE IF NOT EXISTS sys_x (...);
#     CREATE INDEX idx_x_tenant ON sys_x (tenant_id);
#
# Run a second time against a database that holds both, it fails with
# Error 1061, duplicate key name. A second run is a real state: a plugin
# migration runs again whenever its bookkeeping row is missing, which is the
# documented repair for a lost row. PostgreSQL guards
# the same statement with IF NOT EXISTS and SQL Server with a sys.indexes check,
# so only one tree of three cannot re-run.
#
# Two forms re-run cleanly:
#
#   - For a new table, declare the index inside CREATE TABLE as a KEY clause.
#     The table's IF NOT EXISTS then guards the index too.
#   - For an index added to an existing table, read information_schema
#     .statistics first and run the CREATE through PREPARE only when the index
#     is absent. The statement then sits inside a string, which this reads past.
#
# CREATE INDEX IF NOT EXISTS is reported too: MySQL 8 refuses it as a syntax
# error.
#
# Scans this repository, or the tree passed as the first argument, so a plugin
# repository can run it against itself.
#
# Run by CI on every PR. Called from Makefile `make verify`.
set -euo pipefail
export LC_ALL=C

ROOT="$(cd "${1:-$(dirname "$0")/..}" && pwd)"

python3 - "$ROOT" <<'PY'
import os, re, sys

root = sys.argv[1]

STATEMENT = re.compile(r"^\s*CREATE\s+(?:UNIQUE\s+|FULLTEXT\s+|SPATIAL\s+)?INDEX\b", re.I)


def blank(text):
    """Replace comments and quoted strings with spaces, keeping newlines, so a
    statement keeps its line number and a CREATE inside a string or comment is
    not one."""
    out = []
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        nxt = text[i + 1] if i + 1 < n else ""
        if c == "-" and nxt == "-" or c == "#":
            j = text.find("\n", i)
            j = n if j < 0 else j
            out.append(" " * (j - i))
            i = j
        elif c == "/" and nxt == "*":
            j = text.find("*/", i + 2)
            j = n if j < 0 else j + 2
            out.append(re.sub(r"[^\n]", " ", text[i:j]))
            i = j
        elif c in "'\"`":
            j = i + 1
            while j < n:
                if text[j] == "\\":
                    j += 2
                    continue
                if text[j] == c:
                    if j + 1 < n and text[j + 1] == c:
                        j += 2
                        continue
                    break
                j += 1
            j = min(j + 1, n)
            keep = text[i:j] if c == "`" else re.sub(r"[^\n]", " ", text[i:j])
            out.append(keep)
            i = j
        else:
            out.append(c)
            i += 1
    return "".join(out)


scanned = 0
violations = []
for dirpath, dirnames, filenames in os.walk(root):
    dirnames[:] = [d for d in dirnames if not d.startswith(".") and d != "node_modules"]
    if os.path.basename(dirpath) != "mysql" or os.path.basename(os.path.dirname(dirpath)) != "migrations":
        continue
    for name in sorted(filenames):
        if not name.endswith(".up.sql"):
            continue
        path = os.path.join(dirpath, name)
        scanned += 1
        with open(path, encoding="utf-8") as f:
            raw = f.read()
        lines = raw.split("\n")
        code = blank(raw)
        offset = 0
        for stmt in code.split(";"):
            m = STATEMENT.search(stmt)
            if m:
                lead = len(stmt) - len(stmt.lstrip())
                line = code.count("\n", 0, offset + lead) + 1
                violations.append("%s:%d:%s" % (os.path.relpath(path, root), line, lines[line - 1].strip()))
            offset += len(stmt) + 1

if scanned == 0:
    print("::error::no MySQL up migrations found under %s" % root)
    print("Pass the tree to scan: tools/lint-mysql-index-guard.sh /path/to/tree")
    sys.exit(1)

for v in violations:
    print("VIOLATION: " + v)
if violations:
    print("")
    print("ERROR: %d CREATE INDEX statement(s) in MySQL migrations cannot run twice" % len(violations))
    print("MySQL 8 has no CREATE INDEX IF NOT EXISTS, so a second run fails with")
    print("Error 1061 while the PostgreSQL and SQL Server trees re-run cleanly.")
    print("Declare the index inside CREATE TABLE as KEY idx_name (col), or, for an")
    print("existing table, check information_schema.statistics and PREPARE the CREATE.")
    sys.exit(1)

print("OK: %d MySQL up migration(s), no index created by a bare statement" % scanned)
PY
