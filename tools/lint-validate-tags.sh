#!/usr/bin/env bash
# lint-validate-tags.sh - fail when a `validate` tag applies a size constraint
# to a field type that cannot carry one.
#
# go-playground/validator reads max/min/len/gt/lt/gte/lte differently per kind:
# on a string it is a length, on a number it is the value itself, and on a bool
# it is nothing at all. The library does not reject the mismatch. It panics.
#
#   Enabled *bool `validate:"omitempty,max=255"`
#
# omitempty does not save it. On a pointer omitempty means "not nil", so the
# rule runs on any request that mentions the field at all, true or false, and
# validator panics with "Bad field type bool", a 500 on an ordinary PUT. Only
# leaving the key out entirely avoids it, which is why such a field passes
# every test that ignores it.
#
# The numeric case is quieter and just as wrong. A cap copied from a string
# length onto a number caps the value:
#
#   StatusCode *int `validate:"omitempty,max=255"`   # 404 fails validation
#   DurationMs *int `validate:"omitempty,max=255"`   # 256ms fails validation
#
# The same pointer rule applies to the replacement: a status code that never
# arrived is a set pointer holding zero, so the lower bound has to admit it.
#
# A deliberate numeric range is not this mistake and must not be reported. The
# two are told apart by whether the bound is paired: `min=1,max=65535` on a
# port is a range someone chose, while a bare `max=` sitting on one of the
# string-length values below is a length that was copied onto a number.
#
# Fields are read from Go source rather than through reflection, so this runs
# without building anything and covers every struct in the tree. Scanning
# nothing is an error, not a pass.
#
# Usage:
#   tools/lint-validate-tags.sh               # check this repository
#   tools/lint-validate-tags.sh /path/to/tree # check another tree
#
# Run from Makefile `make verify`.
set -euo pipefail
export LC_ALL=C

ROOT="$(cd "${1:-$(dirname "$0")/..}" && pwd)"

python3 - "$ROOT" <<'PY'
import glob, os, re, sys

root = sys.argv[1]

# Values that are string lengths, not quantities. A number carrying one of
# these as a bare upper bound is a string length copied onto a number.
LENGTHS = {"128", "255", "320", "512", "1024", "2048", "4096", "8192", "65535"}

SIZED = ("max", "min", "len", "gt", "lt", "gte", "lte")

BOOL_FIELD = re.compile(r'^\s*(\w+)\s+\*?bool\s+`[^`]*validate:"([^"]*)"')
NUM_FIELD = re.compile(
    r'^\s*(\w+)\s+\*?(?:u?int(?:8|16|32|64)?|float32|float64|time\.Duration)'
    r'\s+`[^`]*validate:"([^"]*)"')

findings = []
fields = 0

for path in sorted(glob.glob(root + "/**/*.go", recursive=True)):
    if path.endswith("_test.go"):
        continue
    src = open(path, encoding="utf-8", errors="replace").read()
    if 'validate:"' not in src:
        continue
    rel = os.path.relpath(path, root)
    for n, text in enumerate(src.split("\n"), start=1):
        m = BOOL_FIELD.match(text)
        if m:
            fields += 1
            rules = [r.split("=")[0] for r in m.group(2).split(",")]
            bad = [r for r in rules if r in SIZED]
            if bad:
                findings.append((
                    "BOOL", "%s:%d" % (rel, n), m.group(1),
                    "%s on a bool panics the validator when the field is true"
                    % ", ".join(sorted(set(bad)))))
            continue

        m = NUM_FIELD.match(text)
        if not m:
            continue
        fields += 1
        rules = m.group(2).split(",")
        caps = [r.split("=", 1)[1] for r in rules
                if r.startswith("max=") and "=" in r]
        paired = any(r.startswith(("min=", "gte=", "gt=")) for r in rules)
        if paired:
            continue
        for cap in caps:
            if cap in LENGTHS:
                findings.append((
                    "NUMBER", "%s:%d" % (rel, n), m.group(1),
                    "max=%s with no lower bound is a string length, and on a "
                    "number it caps the value" % cap))

if not any(True for _ in glob.iglob(root + "/**/*.go", recursive=True)):
    print("::error::no Go source under %s" % root)
    print("Scanning nothing is not a pass.")
    sys.exit(1)

if findings:
    print("::error::%d validate tag(s) constrain a field type that cannot carry "
          "the constraint:" % len(findings))
    print("")
    for kind, where, field, why in findings:
        print("  %-8s %-52s %s" % (kind, where, field))
        print("  %-8s %s" % ("", why))
        print("")
    print("A bool needs no size. A quantity that really does have an upper bound")
    print("states its lower bound too, so the pair reads as a range someone chose.")
    sys.exit(1)

print("OK: %d constrained bool/number field(s), none miscast." % fields)
PY
