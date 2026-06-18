#!/usr/bin/env bash
# check-dockerfile-digests.sh: verify all external base images are digest-pinned
# Returns non-zero if any unpinned external base images are found.
set -euo pipefail

# First-party image patterns. These may use mutable tags (build-time ARG substitution).
FIRST_PARTY_PATTERNS='ghcr.io/lyeve-labs/'

UNPINNED=0
DOCKERFILES=$(find . -name 'Dockerfile*' -not -path '*/node_modules/*' -not -path '*/.git/*' -type f | sort)

for df in $DOCKERFILES; do
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        # Skip comments
        [[ "$line" =~ ^[[:space:]]*# ]] && continue
        
        # Extract the image reference from FROM line
        # Format: FROM [--platform=...] image[:tag|@sha256:...] [AS alias]
        image_ref=$(echo "$line" | sed -n 's/^FROM[[:space:]]\+\(--platform=[^[:space:]]\+[[:space:]]\+\)\?\([^[:space:]]\+\).*//p')
        [[ -z "$image_ref" ]] && continue
        
        # Skip internal stage references (FROM chef, FROM planner, FROM builder, etc.)
        # These reference earlier stages, not external images.
        if [[ ! "$image_ref" =~ [/.] ]]; then
            continue
        fi
        
        # Skip first-party images
        if echo "$image_ref" | grep -q "$FIRST_PARTY_PATTERNS"; then
            continue
        fi
        
        # Skip scratch (no base image)
        [[ "$image_ref" == "scratch" ]] && continue
        
        # Must contain @sha256:
        if ! echo "$image_ref" | grep -q '@sha256:[0-9a-f]\{64\}'; then
            echo "UNPINNED: $image_ref in $df"
            UNPINNED=$((UNPINNED + 1))
        fi
    done < <(grep '^FROM ' "$df" 2>/dev/null || true)
done

echo ""
if [[ "$UNPINNED" -eq 0 ]]; then
    echo "OK: all external base images are digest-pinned"
    exit 0
else
    echo "FAIL: $UNPINNED unpinned external base image(s) found"
    exit 1
fi
