#!/usr/bin/env bash
# Table test for image-tags.sh. CI runs it on every push, so a change to the
# tag rules has to change this table in the same commit.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fail=0

# ref | expected tags, space separated, or REFUSED
cases=(
  "v0.52.0|0.52.0 0.52 latest"
  "v1.0.10|1.0.10 1.0 latest"
  "v0.52.0-rc.1|0.52.0-rc.1"
  "v0.52.0-rc.12|0.52.0-rc.12"
  "v0.52.0-rc.0|REFUSED"
  "v0.52.0-rc.01|REFUSED"
  "v0.52.0-rc|REFUSED"
  "v0.52.0-beta.1|REFUSED"
  "v0.52.0-nightly.20261011.abc1234|REFUSED"
  "v0.52|REFUSED"
  "0.52.0|REFUSED"
  "v01.2.3|REFUSED"
  "v1.2.3.4|REFUSED"
  "|REFUSED"
)

for row in "${cases[@]}"; do
  ref="${row%%|*}"
  want="${row#*|}"
  if got="$(bash "$here/image-tags.sh" "$ref" 2>/dev/null)"; then
    got="$(printf '%s' "$got" | tr '\n' ' ')"
  else
    got="REFUSED"
  fi
  if [ "$got" != "$want" ]; then
    echo "FAIL: '${ref}' gave '${got}', want '${want}'"
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "OK: image-tags.sh, ${#cases[@]} cases"
