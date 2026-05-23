#!/usr/bin/env bash
# lint-no-test-seams.sh - fail if a shipped binary can be made to behave
# differently for a test.
#
# The engine ships one behavior. A test suite gets its way by controlling the
# environment it boots the engine into (a throwaway database, a seeded tenant,
# a fake server it points at), never by asking the binary to relax. The only
# sanctioned per-build variation is a value injected by the linker
# (-X pkg.Symbol=value: version, commit, build date, license public key),
# because it is chosen when the artifact is built and is visible in the
# artifact, not chosen later by whoever sends the request.
#
# A runtime switch fails toward false confidence: a reader sees a control listed
# as disable-able and concludes it is off, and an operator who sets the variable
# in the wrong place turns a control off in production.
#
# What is refused:
#   1. Non-test Go reading an env var shaped like a bypass.
#   2. A build tag named e2e/test/testing, or a file named *_e2e.go.
#   3. A handler branching on a request-supplied injection knob.
#   4. An env-var-shaped literal carrying a bypass word, however it is used.
#      Category 1 reads the name out of the Getenv call, so it cannot see a name
#      assembled at runtime, such as prefix + "_TLS_INSECURE".
#   5. A branch on the production flag (is_production, IsProduction(), an
#      APP_ENV compare, or a bool named for it) whose non-production arm
#      touches an auth, tenant, rate-limit or verification step. The flag is
#      a config value like any other, so "guarded so it cannot fire in
#      production" is a switch a missing APP_ENV flips.
#      Registering an extra service, logging more, or refusing to boot in
#      production only are not weakenings and are not matched. Nor is a
#      production arm that returns early with the relaxed path simply
#      following it: no else, no block to read. A reviewer still has to
#      look for that one.
#
# This scans this repository, or the tree passed as the first argument.
# Suppress a genuine false positive with //lyeve:allow-test-seam plus a reason.
#
# Deliberately not spelled //nolint:...: that namespace belongs to golangci-lint,
# which would read "test-seam" as a linter it does not have. Harmless while
# nolintlint is off and a build failure the day it is turned on.
#
# Run by CI on every PR. Called from Makefile `make verify`.
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
VIOLATIONS=0

report() {
    echo "VIOLATION [$1]: $2"
    VIOLATIONS=$((VIOLATIONS + 1))
}

# Env var names that mean "behave differently for a test". Deliberately narrow:
# CI_DIALECT and DATABASE_URL steer where a test points, which is the sanctioned
# way, and DEBUG/LOG_LEVEL change what is recorded, not what is enforced.
# Each word must be a whole underscore-separated segment. AUTH_ACCOUNT_DISABLED
# and AUTH_LOGIN_SKIPPED are error codes, not switches.
BYPASS_ENV='(^|_)(E2E|SKIP|DISABLE|BYPASS|INSECURE|UNSAFE|NO_AUTH|NOAUTH|ALLOW_ALL|FORCE_FAIL|FAULT_INJECT|STUB|MOCK|FAKE)(_|$)'

# An env-var-shaped literal: all caps, underscore-separated, at least two
# segments, carrying one of the words above. Matched wherever it appears, so a
# name built from parts is caught at the part that carries the word.
BYPASS_LITERAL='^_?[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$'

# The production flag, as a config read, a method, an APP_ENV compare, or a
# bool that carries the word. "product" and "producer" are not matched: the
# identifier ends where the word does.
PROD_IDENT='(is|in)?[pP]rod(uction)?(Mode|Env|Only)?'
# A condition that is true outside production.
PROD_NEG='(![A-Za-z_][A-Za-z0-9_.]*Bool\("is_production"\)|!([A-Za-z_][A-Za-z0-9_]*\.)*'"$PROD_IDENT"'(\(\))?([^A-Za-z0-9_(]|$)|'"$PROD_IDENT"'(\(\))? *(== *false|!= *true)|Bool\("is_production"\) *(== *false|!= *true)|"APP_ENV"\)[^;{]*!= *"[Pp]rod)'
# A condition that is true in production. Its else arm is the seam.
PROD_POS='(Bool\("is_production"\)|([^A-Za-z0-9_!]|^)'"$PROD_IDENT"'(\(\))?([^A-Za-z0-9_(]|$)|"APP_ENV"\)[^;{]*== *"[Pp]rod)'
# What a non-production arm must not touch. Lower-cased before the match.
PROD_WEAKEN='(claims|anonymous|bypass|skip|insecure|allowall|allow_all|noauth|unauth|handler\(ctx|next\.servehttp|ratelimit|rate_limit|limiter|tenant|verify|whitelist|allowlist)'

# Reading a request-supplied knob. Only reads count: a response header that
# carries a correlation ID out is not a switch coming in.
BYPASS_REQ='(Query\(\)\.Get|Header\.Get|FormValue|PathValue)\("[^"]*(force_db_fail|force_fail|e2e|x-test|x-e2e|x-bypass|bypass)[^"]*"\)'

if [ ! -d "$ROOT" ]; then
    echo "::error::not a directory: $ROOT"
    exit 1
fi

while IFS= read -r -d '' file; do
    case "$file" in
        *_test.go|*/vendor/*|*/testdata/*|*/pkg/plugintest/*) continue ;;
    esac
    rel="${file#"$ROOT"/}"

    # 1. Env reads shaped like a bypass.
    hits=$(awk -v pat="$BYPASS_ENV" '
        /\/\/lyeve:allow-test-seam[[:space:]]+[^[:space:]]/ { ok[NR]=1; ok[NR+1]=1 }
        /(os\.Getenv|os\.LookupEnv|envOr|getenv)\(/ {
            if (ok[NR]) next
            line = $0
            while (match(line, /(os\.Getenv|os\.LookupEnv|envOr|getenv)\("[^"]*"/)) {
                hit = substr(line, RSTART, RLENGTH)
                line = substr(line, RSTART + RLENGTH)
                if (match(hit, /"[^"]*"/) && toupper(substr(hit, RSTART + 1, RLENGTH - 2)) ~ pat) {
                    printf "%d:%s\n", NR, $0
                    next
                }
            }
        }
    ' "$file" || true)
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        report env "${rel}:$(echo "$line" | sed 's/[[:space:]]\+/ /g')"
    done <<< "$hits"

    # 2. A build tag or filename that marks a test-only variant.
    case "$file" in
        *_e2e.go) report tag "${rel}: a test-variant source file" ;;
        *)
            if head -5 "$file" | grep -qE '^//go:build .*\b(e2e|testing)\b'; then
                report tag "${rel}: build tag selects a test variant"
            fi
            ;;
    esac

    # 4. An env-var-shaped literal carrying a bypass word.
    hits=$(awk -v pat="$BYPASS_ENV" -v shape="$BYPASS_LITERAL" '
        /\/\/lyeve:allow-test-seam[[:space:]]+[^[:space:]]/ { ok[NR]=1; ok[NR+1]=1 }
        {
            if (ok[NR]) next
            line = $0
            while (match(line, /"[A-Z0-9_]+"/)) {
                lit = substr(line, RSTART + 1, RLENGTH - 2)
                line = substr(line, RSTART + RLENGTH)
                if (lit ~ shape && lit ~ pat) { printf "%d:%s\n", NR, $0; next }
            }
        }
    ' "$file" || true)
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        report literal "${rel}:$(echo "$line" | sed 's/[[:space:]]\+/ /g')"
    done <<< "$hits"

    # 3. Request-supplied injection knobs.
    hits=$(grep -niE "$BYPASS_REQ" "$file" | grep -vE '//lyeve:allow-test-seam' || true)
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        report request "${rel}:$(echo "$line" | sed 's/[[:space:]]\+/ /g')"
    done <<< "$hits"

    # 5. A production-keyed branch whose non-production arm weakens a
    # control. The arm is the negated condition's block, or the else arm
    # of a positive one, read to its closing brace.
    hits=$(awk -v neg="$PROD_NEG" -v pos="$PROD_POS" -v weaken="$PROD_WEAKEN" '
        function braces(s,   o, c) { o = gsub(/\{/, "{", s); c = gsub(/\}/, "}", s); return o - c }
        # The line that opened the block an "} else {" on line i closes.
        function opener(i,   d, j) {
            d = 0
            for (j = i - 1; j >= 1; j--) {
                d -= braces(lines[j])
                if (d < 0) return j
            }
            return 0
        }
        /\/\/lyeve:allow-test-seam[[:space:]]+[^[:space:]]/ { ok[NR] = 1; ok[NR + 1] = 1 }
        { lines[NR] = $0 }
        END {
            for (i = 1; i <= NR; i++) {
                line = lines[i]
                if (ok[i]) continue
                if (line ~ /^[[:space:]]*\/\//) continue
                start = 0
                if (line ~ /^[[:space:]]*(if |\} else if |case |for )/ && line ~ neg) {
                    start = i
                } else if (line ~ /^[[:space:]]*\} else \{/) {
                    j = opener(i)
                    if (j > 0 && !ok[j] && lines[j] ~ /^[[:space:]]*(if |\} else if )/ && lines[j] ~ pos && lines[j] !~ neg) start = i
                }
                if (!start) continue
                depth = 0; found = 0
                for (k = start; k <= NR && k - start <= 60; k++) {
                    l = lines[k]
                    if (k > start && !found && tolower(l) ~ weaken) found = k
                    depth += braces(l)
                    if (k > start && depth <= 0) break
                }
                if (found) printf "%d:%s\n", start, line
            }
        }
    ' "$file" || true)
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        report production "${rel}:$(echo "$line" | sed 's/[[:space:]]\+/ /g')"
    done <<< "$hits"
done < <(find "$ROOT" -name '.worktrees' -prune -o -name '*.go' -not -path '*/.git/*' -print0)


if [ "$VIOLATIONS" -gt 0 ]; then
    echo ""
    echo "ERROR: $VIOLATIONS test seam(s) found."
    echo "A test controls the environment the engine boots into, not the engine."
    echo "If a build genuinely must differ, inject it with -X at link time."
    exit 1
fi
echo "OK: no test seams."
