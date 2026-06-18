#!/usr/bin/env bash
# kernel-smoke.sh boots the kernel binary against an empty Postgres database
# and walks the first run an operator makes: the probes and the container
# healthcheck answer, setup creates the first super admin, that account signs
# in, and the entitlements the admin console reads first report the plan an
# install reports with no license. Then it stops the engine and expects a clean
# exit, after which the healthcheck has to fail.
#
#   DATABASE_URL='postgres://user:pass@host:port/db?sslmode=disable' \
#     scripts/kernel-smoke.sh [path/to/lyeve-core]
#
# The binary defaults to bin/lyeve-core, which make build-kernel writes. The
# database has to be empty, because setup refuses once any account exists.
# SMOKE_ADMIN_PORT and SMOKE_API_PORT pick the two listeners, 3001 and 3002
# when unset.
#
# The engine sees only the settings below. It runs with a clean environment in
# a directory of its own, so a .env or lyeve.yaml in the caller's tree cannot
# change what is being tested. That directory is left in place with the log,
# for reading after a failure.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${1:-$ROOT/bin/lyeve-core}"
: "${DATABASE_URL:?set DATABASE_URL to an empty Postgres database}"
ADMIN_PORT="${SMOKE_ADMIN_PORT:-3001}"
API_PORT="${SMOKE_API_PORT:-3002}"
ADMIN="http://127.0.0.1:$ADMIN_PORT"
API="http://127.0.0.1:$API_PORT"

[ -x "$BIN" ] || { echo "FAIL: $BIN is not executable. Run make build-kernel first." >&2; exit 1; }
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"

WORK="$(mktemp -d)"
LOG="$WORK/engine.log"
BODY="$WORK/body.json"

random_hex() { od -An -tx1 -N32 /dev/urandom | tr -d ' \n'; }
SETUP_TOKEN="$(random_hex)"
EMAIL="admin@example.com"
PASSWORD="Kx9-$(random_hex)"

ENGINE_PID=""
kill_engine() {
  if [ -n "$ENGINE_PID" ] && kill -0 "$ENGINE_PID" 2>/dev/null; then
    kill -KILL "$ENGINE_PID" 2>/dev/null || true
  fi
}
trap kill_engine EXIT

fail() {
  echo "FAIL: $*" >&2
  echo "engine log, last 40 lines of $LOG:" >&2
  tail -n 40 "$LOG" >&2 || true
  exit 1
}

# status prints the HTTP status of one request, 000 when nothing answered, and
# leaves the response body in $BODY.
status() {
  local url=$1
  shift
  curl -sS -o "$BODY" -w '%{http_code}' --max-time 10 "$@" "$url" 2>/dev/null || true
}

# wait_for polls a URL until it answers 200, and fails early when the engine
# has already exited.
wait_for() {
  local url=$1 limit=$2
  local deadline=$((SECONDS + limit))
  while [ "$SECONDS" -lt "$deadline" ]; do
    kill -0 "$ENGINE_PID" 2>/dev/null || fail "the engine exited before $url answered"
    if [ "$(status "$url")" = 200 ]; then
      echo "ok    GET $url (200)"
      return 0
    fi
    sleep 1
  done
  fail "$url did not answer 200 within ${limit}s: $(head -c 300 "$BODY" 2>/dev/null)"
}

# expect sends one request and fails unless it answers the wanted status.
expect() {
  local want=$1 what=$2 url=$3
  shift 3
  local got
  got="$(status "$url" "$@")"
  [ "$got" = "$want" ] || fail "$what answered $got, want $want: $(head -c 300 "$BODY" 2>/dev/null)"
  echo "ok    $what ($got)"
}

# Development, because production refuses the plain HTTP listener and the
# default database credentials a throwaway server has. The environment decides
# only which of those settings the engine accepts, never how a request is
# authorized.
(
  cd "$WORK"
  exec env -i PATH="$PATH" HOME="$WORK" \
    APP_ENV=development \
    DATABASE_URL="$DATABASE_URL" \
    JWT_SECRET="$(random_hex)" \
    ENCRYPTION_KEY="$(random_hex)" \
    LYEVE_SETUP_TOKEN="$SETUP_TOKEN" \
    JWT_KEY_PATH="$WORK/jwt_key.json" \
    LYEVE_LICENSE_CACHE_DIR="$WORK" \
    STORAGE_LOCAL_PATH="$WORK/uploads" \
    MIGRATIONS_PATH="$ROOT/migrations" \
    ADMIN_LISTEN_ADDR="127.0.0.1:$ADMIN_PORT" \
    API_LISTEN_ADDR="127.0.0.1:$API_PORT" \
    "$BIN"
) >"$LOG" 2>&1 &
ENGINE_PID=$!
echo "booting $BIN (pid $ENGINE_PID), admin :$ADMIN_PORT, api :$API_PORT, log $LOG"

wait_for "$ADMIN/healthz" 120
wait_for "$API/healthz" 30
wait_for "$ADMIN/readyz" 60
wait_for "$API/readyz" 60

# The container HEALTHCHECK runs the binary again as a probe of the API port.
env -i PORT="$API_PORT" "$BIN" healthcheck ||
  fail "the healthcheck subcommand exited $? against a running engine, want 0"
echo "ok    healthcheck subcommand against the running engine (exit 0)"

expect 200 "GET /api/admin/setup" "$ADMIN/api/admin/setup"
grep -q '"setup_required":true' "$BODY" ||
  fail "setup reports that an account exists, so the database is not empty: $(head -c 300 "$BODY")"

expect 201 "POST /api/admin/setup" "$ADMIN/api/admin/setup" \
  -H 'Content-Type: application/json' \
  --data "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"setup_token\":\"$SETUP_TOKEN\"}"

expect 200 "GET /api/admin/setup after setup" "$ADMIN/api/admin/setup"
grep -q '"setup_required":false' "$BODY" ||
  fail "setup still reports no account after creating one: $(head -c 300 "$BODY")"

expect 200 "POST /api/admin/auth/login" "$ADMIN/api/admin/auth/login" \
  -H 'Content-Type: application/json' \
  --data "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}"
TOKEN="$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' "$BODY")"
[ -n "$TOKEN" ] || fail "login answered 200 with no session token"

expect 401 "GET /api/admin/entitlements with no session" "$ADMIN/api/admin/entitlements"
expect 200 "GET /api/admin/entitlements" "$ADMIN/api/admin/entitlements" \
  -H "Authorization: Bearer $TOKEN"
# "free" is the plan an install reports with no license.
grep -q '"plan":"free"' "$BODY" ||
  fail "entitlements report a plan other than the unlicensed one: $(head -c 300 "$BODY")"
echo "      $(head -c 300 "$BODY")"

# A clean stop is part of the boot contract: SIGTERM drains, shuts the
# listeners down and exits 0.
kill -TERM "$ENGINE_PID"
deadline=$((SECONDS + 90))
while kill -0 "$ENGINE_PID" 2>/dev/null && [ "$SECONDS" -lt "$deadline" ]; do
  sleep 1
done
kill -0 "$ENGINE_PID" 2>/dev/null && fail "the engine was still running 90s after SIGTERM"
rc=0
wait "$ENGINE_PID" || rc=$?
[ "$rc" = 0 ] || fail "the engine exited $rc after SIGTERM, want 0"
echo "ok    SIGTERM stops the engine with exit 0"

if env -i PORT="$API_PORT" "$BIN" healthcheck; then
  fail "the healthcheck subcommand exited 0 with no engine listening"
fi
echo "ok    healthcheck subcommand with no engine listening (exit 1)"
echo "PASS: the kernel binary boots, sets up, signs in and serves entitlements"
