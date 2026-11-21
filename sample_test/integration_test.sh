#!/usr/bin/env bash
#
# End-to-end integration test for the Feature Flag Service.
#
# It drives the real HTTP API with curl and walks through the whole life of a
# flag: validation, create, evaluate, per-user override, caching and cache
# invalidation, audit trail, delete and 404 behaviour.
#
# Usage:
#   ./sample_test/integration_test.sh                  start a throwaway server, test it, stop it
#   BASE_URL=http://host:8080 ./sample_test/integration_test.sh   test a server that is already running
#
# Environment:
#   BASE_URL   test this server instead of starting one
#   KEEP=1     keep the temp data and log directories of the throwaway server
#
# Requires: bash, curl, jq, and (when starting a server) Go.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACTOR="integration-test"
RUN_ID="$(date +%s)-$$"
FLAG="it-flag-${RUN_ID}"
SERVER_PID=""
WORKDIR=""
PASSED=0
FAILED=0

# ---------- output helpers ----------

if [ -t 1 ]; then
  GREEN=$'\033[32m'; RED=$'\033[31m'; BOLD=$'\033[1m'; RESET=$'\033[0m'
else
  GREEN=""; RED=""; BOLD=""; RESET=""
fi

section() { printf '\n%s== %s ==%s\n' "$BOLD" "$1" "$RESET"; }
pass()    { PASSED=$((PASSED + 1)); printf '  %sPASS%s  %s\n' "$GREEN" "$RESET" "$1"; }
fail()    { FAILED=$((FAILED + 1)); printf '  %sFAIL%s  %s\n' "$RED" "$RESET" "$1"; }

require_tools() {
  for tool in curl jq; do
    command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 2; }
  done
}

# ---------- server lifecycle ----------

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()' 2>/dev/null \
    || echo $((20000 + RANDOM % 20000))
}

start_server() {
  command -v go >/dev/null 2>&1 || { echo "go is required to start a server; set BASE_URL to use an existing one" >&2; exit 2; }
  WORKDIR="$(mktemp -d)"
  local port; port="$(free_port)"
  BASE_URL="http://127.0.0.1:${port}"

  echo "building server..."
  (cd "$ROOT" && go build -o "$WORKDIR/server" ./cmd/server) || { echo "build failed" >&2; exit 2; }

  PORT="$port" DATA_DIR="$WORKDIR/data" LOG_DIR="$WORKDIR/logs" LOG_LEVEL=debug \
    "$WORKDIR/server" >"$WORKDIR/stdout.log" 2>"$WORKDIR/stderr.log" &
  SERVER_PID=$!

  for _ in $(seq 1 100); do
    if curl -fsS "$BASE_URL/readyz" >/dev/null 2>&1; then
      echo "server started on $BASE_URL (pid $SERVER_PID)"
      return
    fi
    kill -0 "$SERVER_PID" 2>/dev/null || { echo "server exited early:" >&2; cat "$WORKDIR/stderr.log" >&2; exit 2; }
    sleep 0.1
  done
  echo "server did not become ready" >&2
  exit 2
}

stop_server() {
  [ -n "$SERVER_PID" ] || return 0
  kill -TERM "$SERVER_PID" 2>/dev/null
  local code=0
  wait "$SERVER_PID" 2>/dev/null || code=$?
  SERVER_PID=""
  return "$code"
}

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill -KILL "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null
  fi
  if [ -n "$WORKDIR" ]; then
    if [ "${KEEP:-0}" = "1" ]; then echo "kept $WORKDIR"; else rm -rf "$WORKDIR"; fi
  fi
}
trap cleanup EXIT

# ---------- request and assertion helpers ----------

STATUS=""
BODY=""
HEADERS=""

# req METHOD PATH [JSON_BODY] [extra curl args...]
# Sends X-Actor on every call. Sets STATUS, BODY and HEADERS.
req() {
  local method="$1" path="$2" data="${3:-}"
  shift; shift; [ $# -gt 0 ] && shift
  local out hdr
  hdr="$(mktemp)"
  local args=(-sS -X "$method" -D "$hdr" -o - -w '\n%{http_code}' -H "X-Actor: $ACTOR")
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data "$data")
  fi
  out="$(curl "${args[@]}" "$@" "$BASE_URL$path")"
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
  HEADERS="$(cat "$hdr")"
  rm -f "$hdr"
}

# req_noactor METHOD PATH [JSON_BODY]: same as req but without X-Actor.
req_noactor() {
  local method="$1" path="$2" data="${3:-}"
  local out args=(-sS -X "$method" -o - -w '\n%{http_code}')
  [ -n "$data" ] && args+=(-H 'Content-Type: application/json' --data "$data")
  out="$(curl "${args[@]}" "$BASE_URL$path")"
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
  HEADERS=""
}

# expect_status DESCRIPTION CODE
expect_status() {
  if [ "$STATUS" = "$2" ]; then pass "$1 -> $2"; else fail "$1 -> expected $2, got $STATUS (body: $BODY)"; fi
}

# expect_json DESCRIPTION JQ_EXPRESSION
# The expression is evaluated against the last response body and must be true.
expect_json() {
  if printf '%s' "$BODY" | jq -e "$2" >/dev/null 2>&1; then
    pass "$1"
  else
    fail "$1 (jq: $2; body: $BODY)"
  fi
}

# evaluate USER -> sets BODY/STATUS for GET /v1/evaluate
evaluate() {
  req GET "/v1/evaluate?flag=${FLAG}&user_id=$1"
}

# ---------- scenario ----------

require_tools
if [ -z "${BASE_URL:-}" ]; then start_server; else echo "testing existing server at $BASE_URL"; fi
echo "flag under test: $FLAG"

section "Health"
req GET /healthz;  expect_status "liveness" 200
req GET /readyz;   expect_status "readiness" 200

section "Validation and authentication"
req_noactor POST /v1/flags "{\"name\":\"${FLAG}\"}"
expect_status "create without X-Actor is rejected" 400
expect_json   "error code is missing_actor" '.error.code == "missing_actor"'

req POST /v1/flags '{"name":"has space"}'
expect_status "flag name with a space is rejected" 400
expect_json   "error code is invalid_request" '.error.code == "invalid_request"'

req POST /v1/flags "{\"name\":\"$(printf 'a%.0s' $(seq 1 65))\"}"
expect_status "flag name over 64 characters is rejected" 400

req POST /v1/flags '{"name":"","description":"x"}'
expect_status "empty flag name is rejected" 400

req POST /v1/flags '{"name":"x","unknown_field":true}'
expect_status "unknown JSON field is rejected" 400

req POST /v1/flags '{not json'
expect_status "malformed JSON is rejected" 400

section "Create and read"
req POST /v1/flags "{\"name\":\"${FLAG}\",\"description\":\"integration test flag\"}"
expect_status "create flag (defaults to disabled)" 201
expect_json   "response is disabled, version 1, no overrides" \
  ".name == \"${FLAG}\" and .enabled == false and .version == 1 and (.overrides | length) == 0"
if printf '%s' "$HEADERS" | grep -qi "^location: /v1/flags/${FLAG}"; then pass "Location header points at the new flag"; else fail "Location header missing"; fi

req POST /v1/flags "{\"name\":\"${FLAG}\"}"
expect_status "creating the same flag again" 409
expect_json   "error code is already_exists" '.error.code == "already_exists"'

req GET "/v1/flags/${FLAG}"
expect_status "get flag" 200
expect_json   "description round-trips" '.description == "integration test flag"'

req GET /v1/flags
expect_status "list flags" 200
expect_json   "list contains the flag" "[.flags[].name] | index(\"${FLAG}\") != null"

section "Evaluation: global state"
evaluate alice
expect_status "evaluate while globally off" 200
expect_json   "disabled, reason global_off, served from store" \
  '.enabled == false and .reason == "global_off" and .source == "store"'

section "Evaluation: per-user override beats global state"
req PUT "/v1/flags/${FLAG}/overrides/alice" '{"enabled":true}'
expect_status "set override for alice" 200
expect_json   "response shows the override" '.overrides.alice == true'

evaluate alice
expect_json "alice is enabled by her override (global is off)" \
  '.enabled == true and .reason == "user_override"'
evaluate alice
expect_json "second evaluation is served from the cache" '.enabled == true and .source == "cache"'

evaluate bob
expect_json "bob still follows the global state" '.enabled == false and .reason == "global_off"'

section "Cache invalidation on write"
req PUT "/v1/flags/${FLAG}" '{"enabled":true}'
expect_status "enable the flag globally" 200
expect_json   "version advanced to 3" '.enabled == true and .version == 3'

evaluate bob
expect_json "bob sees the new global state, not a stale cached answer" \
  '.enabled == true and .reason == "global_on" and .source == "store"'
evaluate bob
expect_json "and it is cached again afterwards" '.enabled == true and .source == "cache"'

req PUT "/v1/flags/${FLAG}/overrides/alice" '{"enabled":false}'
expect_status "flip alice's override to off while the flag is on" 200
evaluate alice
expect_json "override off beats global on" '.enabled == false and .reason == "user_override"'

req DELETE "/v1/flags/${FLAG}/overrides/alice"
expect_status "remove alice's override" 204
evaluate alice
expect_json "alice falls back to the global state" '.enabled == true and .reason == "global_on"'

req PUT "/v1/flags/${FLAG}" '{"enabled":false,"description":"updated description"}'
expect_status "disable the flag globally and change the description" 200
evaluate bob
expect_json "bob's cached 'on' answer was invalidated" '.enabled == false and .source == "store"'

section "Override validation"
req PUT "/v1/flags/${FLAG}/overrides/alice" '{}'
expect_status "override without 'enabled' is rejected" 400
req PUT "/v1/flags/does-not-exist-${RUN_ID}/overrides/alice" '{"enabled":true}'
expect_status "override on a missing flag" 404

section "Audit trail"
req GET "/v1/flags/${FLAG}/audit"
expect_status "read audit trail" 200
expect_json   "six entries recorded" '.entries | length == 6'
expect_json   "newest first: last change was the update" '.entries[0].action == "flag.updated"'
expect_json   "oldest entry is the creation, with no previous value" \
  '.entries[-1].action == "flag.created" and .entries[-1].before == null and .entries[-1].after.name != null'
expect_json   "actions recorded in order" \
  '[.entries[].action] | reverse == ["flag.created","override.set","flag.updated","override.set","override.removed","flag.updated"]'
expect_json   "every entry records who made the change" '[.entries[].actor] | all(. == "integration-test")'
expect_json   "every entry has a timestamp" '[.entries[].at] | all(. != null and . != "")'
expect_json   "update entry holds previous and new value" \
  '.entries[0].before.enabled == true and .entries[0].after.enabled == false'

req GET "/v1/flags/${FLAG}/audit?limit=2"
expect_json   "limit parameter is honoured" '.entries | length == 2'

section "Request id"
req GET /healthz "" -H "X-Request-ID: it-req-${RUN_ID}"
if printf '%s' "$HEADERS" | grep -qi "^x-request-id: it-req-${RUN_ID}"; then pass "client request id is echoed back"; else fail "request id not echoed"; fi
req GET /healthz
if printf '%s' "$HEADERS" | grep -qi "^x-request-id: ."; then pass "a request id is generated when none is sent"; else fail "no generated request id"; fi

section "Not found is never a 500"
GHOST="ghost-${RUN_ID}"
req GET "/v1/flags/${GHOST}";                    expect_status "get unknown flag" 404
expect_json "error body has code not_found and a request id" '.error.code == "not_found" and (.error.request_id | length) > 0'
req PUT "/v1/flags/${GHOST}" '{"enabled":true}'; expect_status "update unknown flag" 404
req DELETE "/v1/flags/${GHOST}";                 expect_status "delete unknown flag" 404
req GET "/v1/evaluate?flag=${GHOST}&user_id=alice"; expect_status "evaluate unknown flag" 404
req GET "/v1/flags/${GHOST}/audit"
expect_status "audit of unknown flag is an empty list, not an error" 200
expect_json   "empty entries" '.entries | length == 0'

section "Evaluate input validation"
req GET "/v1/evaluate?user_id=alice"; expect_status "missing flag parameter" 400
req GET "/v1/evaluate?flag=${FLAG}";  expect_status "missing user_id parameter" 400

section "Routing"
req GET /v1/nope;           expect_status "unknown route" 404
expect_json "unknown route answers in the JSON error shape" '.error.code == "not_found"'
req PATCH "/v1/flags/${FLAG}"; expect_status "wrong method" 405
if printf '%s' "$HEADERS" | grep -qi '^allow:'; then pass "405 carries an Allow header"; else fail "405 without Allow header"; fi

section "Delete"
evaluate alice
evaluate alice
expect_json "alice's answer is cached just before the delete" '.source == "cache"'
req DELETE "/v1/flags/${FLAG}";  expect_status "delete flag" 204
req GET "/v1/flags/${FLAG}";     expect_status "flag is gone" 404
evaluate alice;                  expect_status "evaluating a deleted flag is 404, not a stale cached answer" 404
req DELETE "/v1/flags/${FLAG}";  expect_status "deleting twice" 404
req GET "/v1/flags/${FLAG}/audit"
expect_status "audit trail outlives the flag" 200
expect_json   "newest entry records the deletion with the final state as 'before'" \
  '.entries[0].action == "flag.deleted" and .entries[0].after == null and .entries[0].before.enabled == false'
expect_json   "full history is still there (7 entries)" '.entries | length == 7'

section "Persistence"
if [ -n "$SERVER_PID" ]; then
  PERSIST="persist-${RUN_ID}"
  req POST /v1/flags "{\"name\":\"${PERSIST}\",\"enabled\":true}"
  expect_status "create a flag to survive a restart" 201

  if stop_server; then pass "server shut down gracefully on SIGTERM (exit 0)"; else fail "server did not exit 0 on SIGTERM"; fi
  grep -q "server stopped cleanly" "$WORKDIR/stdout.log" && pass "shutdown sequence was logged" || fail "no clean-shutdown log line"

  PORT="${BASE_URL##*:}" DATA_DIR="$WORKDIR/data" LOG_DIR="$WORKDIR/logs" \
    "$WORKDIR/server" >>"$WORKDIR/stdout.log" 2>>"$WORKDIR/stderr.log" &
  SERVER_PID=$!
  for _ in $(seq 1 100); do curl -fsS "$BASE_URL/readyz" >/dev/null 2>&1 && break; sleep 0.1; done

  req GET "/v1/flags/${PERSIST}"
  expect_status "flag survived the restart" 200
  expect_json   "state is intact" '.enabled == true'
  req GET "/v1/flags/${PERSIST}/audit"
  expect_json   "audit trail survived the restart" '(.entries | length) == 1 and .entries[0].action == "flag.created"'

  if [ -s "$WORKDIR/logs/app.log" ]; then pass "log file was written"; else fail "log file is empty"; fi
  req DELETE "/v1/flags/${PERSIST}"
else
  echo "  (skipped: restart checks need the script to own the server; unset BASE_URL)"
fi

# ---------- summary ----------

printf '\n%s%d passed, %d failed%s\n' "$BOLD" "$PASSED" "$FAILED" "$RESET"
[ "$FAILED" -eq 0 ]
