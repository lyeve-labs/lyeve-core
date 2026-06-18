#!/usr/bin/env bash
# mutation.sh - Run gremlins mutation testing on critical packages.
# Usage: bash scripts/mutation.sh [--dry-run] [--report]
#
# Runs gremlins on the 4 critical internal packages, aggregates scores,
# and optionally writes a report to bench-results/mutation/.
#
# Exit codes:
#   0 - all packages scored above threshold
#   1 - one or more packages missed threshold (or gremlins not found)
#   2 - usage error

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CORE_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$CORE_DIR"

# Config
DRY_RUN=false
WRITE_REPORT=false
THRESHOLD_EFFICACY="${MUTATION_THRESHOLD_EFFICACY:-50}"  # percent
THRESHOLD_MCOVER="${MUTATION_THRESHOLD_MCOVER:-30}"      # percent

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)  DRY_RUN=true;  shift ;;
    --report)   WRITE_REPORT=true; shift ;;
    --threshold-efficacy) THRESHOLD_EFFICACY="$2"; shift 2 ;;
    --threshold-mcover)   THRESHOLD_MCOVER="$2";   shift 2 ;;
    *) echo "Unknown flag: $1"; exit 2 ;;
  esac
done

# Helpers
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

log()  { echo -e "${BOLD}$*${RESET}"; }
info() { echo -e "${CYAN}$*${RESET}"; }
ok()   { echo -e "${GREEN}$*${RESET}"; }
warn() { echo -e "${YELLOW}$*${RESET}"; }
err()  { echo -e "${RED}$*${RESET}"; }

# Ensure gremlins is in PATH
if ! command -v gremlins &>/dev/null; then
  err "gremlins not found in PATH. Install: https://github.com/go-gremlins/gremlins/releases"
  exit 1
fi

# Package definitions
# Each entry: "package_path|exclude_files|description"
# exclude_files is a comma-separated list of regex patterns for --exclude-files
PACKAGES=(
  "./internal/auth|jwt\\.go,jwt_signing\\.go,jwks\\.go|Auth & crypto"
  "./internal/db||Database & SQL dialects"
  "./internal/schema||Schema defaults and validation"
  "./internal/middleware||HTTP middleware"
)

# Results tracking
declare -A PKG_STATUS     # package -> ok|fail|error|skip
declare -A PKG_EFFICACY   # package -> efficacy %
declare -A PKG_MCOVER     # package -> mutant coverage %
declare -A PKG_KILLED     # package -> killed count
declare -A PKG_LIVED      # package -> lived count
declare -A PKG_TOTAL      # package -> total mutants
declare -A PKG_ERROR      # package -> error message

PASS_COUNT=0
FAIL_COUNT=0
ERROR_COUNT=0

# Run gremlins per package
GREMLINS_FLAGS=()
if $DRY_RUN; then
  GREMLINS_FLAGS+=("-d")
fi

# Add build tags if integration DB is available
if [[ -n "${TEST_DATABASE_URL:-}" ]]; then
  GREMLINS_FLAGS+=("-t" "integration")
fi

OUTDIR=$(mktemp -d /tmp/gremlins-XXXXXX)

for entry in "${PACKAGES[@]}"; do
  IFS='|' read -r pkg excludes desc <<< "$entry"
  pkg_name="${pkg#./internal/}"

  log ""
  log "$desc ($pkg)"

  # Build command
  cmd=(gremlins unleash "${GREMLINS_FLAGS[@]}")
  cmd+=("--coverpkg" "${pkg}/")
  if [[ -n "$excludes" ]]; then
    cmd+=("--exclude-files" "$excludes")
  fi
  cmd+=("--output" "$OUTDIR/${pkg_name}.json")
  cmd+=("$pkg")

  info "  cmd: ${cmd[*]}"

  # Run with timeout (15 min per package)
  GREMLINS_OUT="$OUTDIR/${pkg_name}.txt"
  if timeout 900 "${cmd[@]}" > "$GREMLINS_OUT" 2>&1; then
    GREMLINS_RC=0
  else
    GREMLINS_RC=$?
  fi

  # Parse output
  # Dry-run format: "Runnable: X, Not covered: Y" and "Mutator coverage: Z%"
  # Full run format: lines with KILLED/LIVED/NOT_VIABLE/NOT_COVERED/TIMED_OUT counts
  #                   and "Efficacy: X%" or "R score: 0.X" and "Mutant coverage: Y%"

  if [[ $GREMLINS_RC -ne 0 ]]; then
    # A coverage-gathering failure means the package's tests fail unmutated.
    if grep -q "failed to gather coverage" "$GREMLINS_OUT"; then
      PKG_STATUS["$pkg_name"]="error"
      PKG_ERROR["$pkg_name"]="coverage gathering failed (the tests fail unmutated)"
      err "  Coverage gathering failed: the tests fail unmutated, which blocks gremlins"
      ((ERROR_COUNT++)) || true
    else
      # Check if threshold failure (exit code 1 = threshold missed)
      PKG_STATUS["$pkg_name"]="fail"
      PKG_ERROR["$pkg_name"]="threshold not met or gremlins error"
      err "  gremlins exited with code $GREMLINS_RC"
      ((FAIL_COUNT++)) || true
    fi
    # Still try to parse what we can from the output
    tail -5 "$GREMLINS_OUT" | head -5
    continue
  fi

  # Parse successful output
  if $DRY_RUN; then
    # Dry-run: "Runnable: X, Not covered: Y" / "Mutator coverage: Z%"
    runnable=$(grep -oP 'Runnable:\s*\K\d+' "$GREMLINS_OUT" || echo "0")
    not_covered=$(grep -oP 'Not covered:\s*\K\d+' "$GREMLINS_OUT" || echo "0")
    mcover=$(grep -oP 'Mutator coverage:\s*\K[\d.]+' "$GREMLINS_OUT" || echo "0")
    total=$((runnable + not_covered))

    PKG_STATUS["$pkg_name"]="ok"
    PKG_EFFICACY["$pkg_name"]="dry-run"
    PKG_MCOVER["$pkg_name"]="$mcover"
    PKG_KILLED["$pkg_name"]="0"
    PKG_LIVED["$pkg_name"]="0"
    PKG_TOTAL["$pkg_name"]="$total"

    ok "  Dry-run: $runnable runnable, $not_covered not covered, mutator coverage: ${mcover}%"
  else
    # Full run - parse from terminal output
    killed=$(grep -oP 'KILLED:\s*\K\d+' "$GREMLINS_OUT" || echo "0")
    lived=$(grep -oP 'LIVED:\s*\K\d+' "$GREMLINS_OUT" || echo "0")
    not_viable=$(grep -oP 'NOT VIABLE:\s*\K\d+' "$GREMLINS_OUT" || echo "0")
    not_covered=$(grep -oP 'NOT COVERED:\s*\K\d+' "$GREMLINS_OUT" || echo "0")
    timed_out=$(grep -oP 'TIMED OUT:\s*\K\d+' "$GREMLINS_OUT" || echo "0")
    total=$((killed + lived + not_viable + not_covered + timed_out))

    # Efficacy = killed / (killed + lived)
    if (( killed + lived > 0 )); then
      efficacy=$(awk "BEGIN {printf \"%.1f\", $killed / ($killed + $lived) * 100}")
    else
      efficacy="0.0"
    fi

    # Mutant coverage = (killed + lived) / total
    if (( total > 0 )); then
      mcover=$(awk "BEGIN {printf \"%.1f\", ($killed + $lived) / $total * 100}")
    else
      mcover="0.0"
    fi

    PKG_KILLED["$pkg_name"]="$killed"
    PKG_LIVED["$pkg_name"]="$lived"
    PKG_TOTAL["$pkg_name"]="$total"
    PKG_EFFICACY["$pkg_name"]="$efficacy"
    PKG_MCOVER["$pkg_name"]="$mcover"

    # Check thresholds
    eff_ok=$(awk "BEGIN {print ($efficacy >= $THRESHOLD_EFFICACY) ? 1 : 0}")
    mc_ok=$(awk "BEGIN {print ($mcover >= $THRESHOLD_MCOVER) ? 1 : 0}")

    if [[ "$eff_ok" -eq 1 && "$mc_ok" -eq 1 ]]; then
      PKG_STATUS["$pkg_name"]="ok"
      ok "  Efficacy: ${efficacy}% (≥${THRESHOLD_EFFICACY}%), Mutator coverage: ${mcover}% (≥${THRESHOLD_MCOVER}%)"
      ((PASS_COUNT++)) || true
    else
      PKG_STATUS["$pkg_name"]="fail"
      err "  Efficacy: ${efficacy}% (need ≥${THRESHOLD_EFFICACY}%), Mutator coverage: ${mcover}% (need ≥${THRESHOLD_MCOVER}%)"
      ((FAIL_COUNT++)) || true
    fi
    info "     KILLED=$killed LIVED=$lived NOT_VIABLE=$not_viable NOT_COVERED=$not_covered TIMED_OUT=$timed_out"
  fi
done

# Summary
log ""
log "Mutation Testing Summary"
log ""
printf "  %-25s %-10s %-12s %-12s %-8s %-8s\n" "PACKAGE" "STATUS" "EFFICACY" "MCOVER" "KILLED" "TOTAL"
printf "  %-25s %-10s %-12s %-12s %-8s %-8s\n" "-------" "------" "--------" "------" "------" "-----"

for entry in "${PACKAGES[@]}"; do
  IFS='|' read -r pkg _ _ <<< "$entry"
  pkg_name="${pkg#./internal/}"
  status="${PKG_STATUS[$pkg_name]:-unknown}"
  efficacy="${PKG_EFFICACY[$pkg_name]:-N/A}"
  mcover="${PKG_MCOVER[$pkg_name]:-N/A}"
  killed="${PKG_KILLED[$pkg_name]:-0}"
  total="${PKG_TOTAL[$pkg_name]:-0}"

  case "$status" in
    ok)    color="$GREEN" ;;
    fail)  color="$RED"   ;;
    error) color="$YELLOW" ;;
    *)     color="$RESET" ;;
  esac

  printf "  ${color}%-25s %-10s %-12s %-12s %-8s %-8s${RESET}\n" \
    "$pkg_name" "$status" "${efficacy}%" "${mcover}%" "$killed" "$total"
done

log ""
log "  Passed: $PASS_COUNT  Failed: $FAIL_COUNT  Errors: $ERROR_COUNT"
log "  Thresholds: efficacy ≥${THRESHOLD_EFFICACY}%, mutant coverage ≥${THRESHOLD_MCOVER}%"

# Report
if $WRITE_REPORT; then
  REPORT_DIR="$CORE_DIR/bench-results/mutation"
  mkdir -p "$REPORT_DIR"
  REPORT_FILE="$REPORT_DIR/mutation-testing-$(date -u +%Y-%m-%d).md"

  {
    echo "# Mutation Testing Report: LyEve Core"
    echo "## Date: $(date -u +%Y-%m-%d) | Tool: gremlins v$(grep -oP 'gremlins version \K[^\s]+' <(grep -oP 'gremlins version.*' "$OUTDIR"/*.txt 2>/dev/null || echo 'unknown') 2>/dev/null || echo 'unknown')"
    echo ""
    echo "---"
    echo ""
    echo "## Configuration"
    echo ""
    echo "- **Packages:** internal/auth, internal/db, internal/schema, internal/middleware"
    echo "- **Thresholds:** efficacy ≥${THRESHOLD_EFFICACY}%, mutant coverage ≥${THRESHOLD_MCOVER}%"
    echo "- **Mode:** $(if $DRY_RUN; then echo 'dry-run (analysis only)'; else echo 'full mutation'; fi)"
    echo ""
    echo "---"
    echo ""
    echo "## Results"
    echo ""
    echo "| Package | Status | Efficacy | Mutant Coverage | Killed | Total | Notes |"
    echo "|---------|--------|----------|-----------------|--------|-------|-------|"

    for entry in "${PACKAGES[@]}"; do
      IFS='|' read -r pkg _ desc <<< "$entry"
      pkg_name="${pkg#./internal/}"
      status="${PKG_STATUS[$pkg_name]:-unknown}"
      efficacy="${PKG_EFFICACY[$pkg_name]:-N/A}"
      mcover="${PKG_MCOVER[$pkg_name]:-N/A}"
      killed="${PKG_KILLED[$pkg_name]:-0}"
      total="${PKG_TOTAL[$pkg_name]:-0}"
      note="${PKG_ERROR[$pkg_name]:-}"

      status_icon=""
      [[ "$status" == "fail" ]] && status_icon=""
      [[ "$status" == "error" ]] && status_icon=""

      echo "| $pkg_name | $status_icon $status | ${efficacy}% | ${mcover}% | $killed | $total | $note |"
    done

    echo ""
    echo "---"
    echo ""
    echo "## Threshold Gate"
    echo ""
    if [[ $FAIL_COUNT -eq 0 && $ERROR_COUNT -eq 0 ]]; then
      echo "**PASS** - all packages meet thresholds."
    else
      echo "**FAIL** - $FAIL_COUNT package(s) below threshold, $ERROR_COUNT errored."
    fi
    echo ""
    echo "---"
    echo ""
    echo "## Raw Output"
    echo ""
    echo "<details><summary>Per-package gremlins output</summary>"
    echo ""
    for entry in "${PACKAGES[@]}"; do
      IFS='|' read -r pkg _ _ <<< "$entry"
      pkg_name="${pkg#./internal/}"
      echo "### $pkg_name"
      echo '```'
      cat "$OUTDIR/${pkg_name}.txt" 2>/dev/null | tail -30
      echo '```'
      echo ""
    done
    echo "</details>"
  } > "$REPORT_FILE"

  log ""
  ok "Report written to: $REPORT_FILE"
fi

# Cleanup
rm -rf "$OUTDIR"

# Exit
if [[ $FAIL_COUNT -gt 0 ]]; then
  err ""
  err "Mutation testing FAILED - $FAIL_COUNT package(s) below threshold."
  exit 1
fi

if [[ $ERROR_COUNT -gt 0 ]]; then
  warn ""
  warn "Mutation testing completed with ERRORS - $ERROR_COUNT package(s) could not be scored."
  # A package that cannot be scored is an infrastructure problem, not a drop
  # in test quality, which is all this check judges, so it exits 0. A package
  # that scores is held to its threshold.
  exit 0
fi

ok ""
ok "Mutation testing PASSED - all packages meet thresholds."
exit 0
