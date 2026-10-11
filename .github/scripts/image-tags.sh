#!/usr/bin/env bash
# Print the image tags a release ref publishes, one per line, or refuse the ref.
#
#   image-tags.sh v1.2.3        1.2.3, 1.2 and latest
#   image-tags.sh v1.2.3-rc.1   1.2.3-rc.1 only
#
# A release candidate is published under its exact version and nothing else.
# Pushing it as latest or as the minor line would hand every install that
# follows either one a build nobody has signed off. Any other shape is refused,
# because a tag the workflow cannot classify must not publish at all.
#
# The release workflow calls this for both the validation and the tag list, so
# the rules live in one place and image-tags_test.sh can hold them.
set -euo pipefail

ref="${1:-}"
num='(0|[1-9][0-9]*)'

if [[ "$ref" =~ ^v${num}\.${num}\.${num}$ ]]; then
  version="${ref#v}"
  printf '%s\n' "$version" "${version%.*}" latest
elif [[ "$ref" =~ ^v${num}\.${num}\.${num}-rc\.[1-9][0-9]*$ ]]; then
  printf '%s\n' "${ref#v}"
else
  echo "image-tags: '${ref}' is neither vX.Y.Z nor vX.Y.Z-rc.N (N from 1)" >&2
  exit 1
fi
