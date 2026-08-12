#!/usr/bin/env bash
#
# Contract smoke test. Everything docs/CONTRACT.md promises is checked here
# against a running stack, with real requests to real providers.
#
# The capability matrix in config/models.yaml is a declaration, not detection:
# this script is what turns a wrong declaration into a failing build.
#
# Usage:
#   make smoke
#   GATEWAY_URL=https://gw.example.com GW_KEY=sk-gw-… ./test/smoke/smoke.sh
#
# Environment:
#   GATEWAY_URL        gateway base URL (default http://localhost:4000)
#   GW_KEY             key of SMOKE_CONSUMER; issued through gwctl when unset
#   SMOKE_CONSUMER     consumer to test with (default tansultant-reactivation)
#   SMOKE_ALIAS        chat alias the key may use (default balanced)
#   SMOKE_DENIED_ALIAS alias the key may NOT use (default smart)
#   SMOKE_DESTRUCTIVE  1 to also test revoke and rotate (mutates keys)
#   SMOKE_BUDGET       1 to test budget exhaustion (spends until the cap)
#   SMOKE_FALLBACK     1 to test fallback (needs a broken primary target)

set -uo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://localhost:4000}"
SMOKE_CONSUMER="${SMOKE_CONSUMER:-tansultant-reactivation}"
SMOKE_ALIAS="${SMOKE_ALIAS:-balanced}"
SMOKE_DENIED_ALIAS="${SMOKE_DENIED_ALIAS:-smart}"
GWCTL="${GWCTL:-./bin/gwctl}"
COMPOSE="${COMPOSE:-docker compose -f deploy/docker-compose.yml --env-file deploy/.env}"

passed=0
failed=0
skipped=0

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; passed=$((passed + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; [ $# -gt 1 ] && printf '       %s\n' "$2"; failed=$((failed + 1)); }
skip() { printf '  \033[33mSKIP\033[0m %s\n' "$1"; [ $# -gt 1 ] && printf '       %s\n' "$2"; skipped=$((skipped + 1)); }
section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "$1 is required but not installed"; exit 2; }
}
require curl
require jq

# body_file / head_file hold the last response.
body_file="$(mktemp)"
head_file="$(mktemp)"
trap 'rm -f "$body_file" "$head_file"' EXIT

# call METHOD PATH [DATA] -> prints the status code, fills body/head files.
call() {
  local method="$1" path="$2" data="${3:-}"
  local args=(-sS -o "$body_file" -D "$head_file" -w '%{http_code}'
              -X "$method" "${GATEWAY_URL}${path}"
              -H "Authorization: Bearer ${GW_KEY}"
              --max-time 45)
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' -d "$data")
  fi
  curl "${args[@]}"
}

header_value() {
  # Header names are case-insensitive on the wire.
  tr -d '\r' < "$head_file" | grep -i "^$1:" | tail -1 | cut -d' ' -f2-
}

# ---------------------------------------------------------------- setup

section "Setup"

if [ -z "${GW_KEY:-}" ]; then
  if [ ! -x "$GWCTL" ]; then
    echo "GW_KEY is not set and $GWCTL is not built. Run: make build"
    exit 2
  fi
  echo "  issuing a key for ${SMOKE_CONSUMER}…"
  GW_KEY="$("$GWCTL" key issue "$SMOKE_CONSUMER" --output json 2>/dev/null | jq -r '.key // empty')"
  if [ -z "$GW_KEY" ]; then
    echo "  could not issue a key. Pass an existing one: GW_KEY=sk-gw-… make smoke"
    exit 2
  fi
fi

case "$GW_KEY" in
  sk-gw-*) pass "key follows the sk-gw-<consumer>-<random> contract" ;;
  *)       fail "key does not follow the contract" "got: ${GW_KEY:0:12}…" ;;
esac

# ------------------------------------------------------------- liveness

section "Health endpoints (no auth)"

status="$(curl -sS -o /dev/null -w '%{http_code}' "${GATEWAY_URL}/health/liveness" --max-time 10)"
[ "$status" = "200" ] && pass "GET /health/liveness is public and returns 200" \
                      || fail "GET /health/liveness returned $status"

status="$(curl -sS -o "$body_file" -w '%{http_code}' "${GATEWAY_URL}/health/readiness" --max-time 10)"
[ "$status" = "200" ] && pass "GET /health/readiness is public and returns 200" \
                      || fail "GET /health/readiness returned $status" "$(head -c 300 "$body_file")"

# --------------------------------------------------------------- models

section "GET /v1/models (acceptance 12)"

status="$(call GET /v1/models)"
if [ "$status" != "200" ]; then
  fail "GET /v1/models returned $status" "$(head -c 300 "$body_file")"
else
  aliases="$(jq -r '.data[].id' "$body_file" | sort | tr '\n' ' ')"
  pass "GET /v1/models returns: ${aliases}"

  if jq -e --arg alias "$SMOKE_DENIED_ALIAS" '.data[] | select(.id == $alias)' "$body_file" >/dev/null; then
    fail "an alias outside the key whitelist is listed" "$SMOKE_DENIED_ALIAS"
  else
    pass "only the key's own aliases are listed"
  fi

  if jq -e '.data[] | select(.id | contains("--fallback-"))' "$body_file" >/dev/null; then
    fail "internal fallback groups leak into the public model list"
  else
    pass "internal fallback groups are not exposed"
  fi
fi

# ----------------------------------------------------------------- chat

section "POST /v1/chat/completions (acceptance 4)"

status="$(call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_ALIAS" '{
  model: $model,
  max_tokens: 32,
  messages: [{role: "user", content: "Reply with the single word: pong"}]
}')")"

if [ "$status" != "200" ]; then
  fail "chat request returned $status" "$(head -c 400 "$body_file")"
else
  pass "chat request on '${SMOKE_ALIAS}' returns 200"

  tokens="$(jq -r '.usage.total_tokens // 0' "$body_file")"
  [ "$tokens" -gt 0 ] && pass "usage is present and non-empty (${tokens} tokens)" \
                      || fail "usage is missing or zero" "$(head -c 300 "$body_file")"

  for header in x-gw-model x-gw-alias x-gw-request-id; do
    value="$(header_value "$header")"
    [ -n "$value" ] && pass "response header ${header}: ${value}" \
                    || fail "response header ${header} is missing"
  done
fi

# ------------------------------------------------------------ streaming

section "Streaming (acceptance 9)"

stream_file="$(mktemp)"
curl -sS -N --max-time 45 \
  -H "Authorization: Bearer ${GW_KEY}" -H 'Content-Type: application/json' \
  -X POST "${GATEWAY_URL}/v1/chat/completions" \
  -d "$(jq -nc --arg model "$SMOKE_ALIAS" '{
        model: $model, stream: true, max_tokens: 32,
        messages: [{role: "user", content: "Count to three."}]
      }')" > "$stream_file"

if grep -q '^data: ' "$stream_file"; then
  pass "streaming response is delivered as SSE"
else
  fail "no SSE frames received" "$(head -c 300 "$stream_file")"
fi

# The gateway forces stream_options.include_usage even though the request
# above does not ask for it (contract §3.4).
if grep '^data: ' "$stream_file" | sed 's/^data: //' | grep -v '^\[DONE\]$' \
     | jq -e -s 'map(select(.usage != null and .usage.total_tokens > 0)) | length > 0' >/dev/null 2>&1; then
  pass "usage is present in the streamed response without the client asking"
else
  fail "no usage in the streamed response" "stream_options.include_usage was not forced"
fi
rm -f "$stream_file"

# ---------------------------------------------------------------- tools

section "Tools (acceptance 10)"

status="$(call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_ALIAS" '{
  model: $model,
  max_tokens: 256,
  messages: [{role: "user", content: "What is the weather in Berlin? Use the tool."}],
  tools: [{
    type: "function",
    function: {
      name: "get_weather",
      description: "Get the current weather in a city",
      parameters: {
        type: "object",
        properties: {city: {type: "string"}},
        required: ["city"]
      }
    }
  }],
  tool_choice: "auto"
}')")"

if [ "$status" != "200" ]; then
  fail "tools request returned $status" "$(head -c 400 "$body_file")"
elif jq -e '.choices[0].message.tool_calls[0].function.name == "get_weather"' "$body_file" >/dev/null; then
  pass "tool_calls returned with the expected function"
else
  fail "no tool_calls in the response" "$(jq -c '.choices[0].message' "$body_file")"
fi

# -------------------------------------------------------------- contract

section "Contract errors (acceptance 5, 6)"

status="$(call POST /v1/chat/completions '{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}')"
if [ "$status" = "400" ]; then
  pass "a vendor model name is rejected with 400"
  code="$(jq -r '.error.code // .detail.error.code // empty' "$body_file")"
  [ "$code" = "unknown_model_alias" ] && pass "error.code is unknown_model_alias" \
                                      || fail "unexpected error code" "$(head -c 300 "$body_file")"
  if jq -e '(.available // .detail.available // []) | length > 0' "$body_file" >/dev/null; then
    pass "the error lists the aliases the caller may use"
  else
    fail "the error does not list available aliases" "$(head -c 300 "$body_file")"
  fi
else
  fail "a vendor model name returned $status, expected 400" "$(head -c 300 "$body_file")"
fi

status="$(call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_DENIED_ALIAS" '{
  model: $model, max_tokens: 16, messages: [{role: "user", content: "hi"}]
}')")"
if [ "$status" = "403" ]; then
  pass "an alias outside the key whitelist is rejected with 403"
  code="$(jq -r '.error.code // .detail.error.code // empty' "$body_file")"
  [ "$code" = "alias_not_permitted" ] && pass "error.code is alias_not_permitted" \
                                      || fail "unexpected error code" "$(head -c 300 "$body_file")"
else
  fail "a forbidden alias returned $status, expected 403" "$(head -c 300 "$body_file")"
fi

status="$(curl -sS -o "$body_file" -w '%{http_code}' --max-time 20 \
  -H "Authorization: Bearer sk-gw-nobody-0000000000000000000000000000000000000000" \
  -H 'Content-Type: application/json' \
  -X POST "${GATEWAY_URL}/v1/chat/completions" \
  -d '{"model":"'"$SMOKE_ALIAS"'","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}')"
[ "$status" = "401" ] && pass "an unknown key is rejected with 401" \
                      || fail "an unknown key returned $status, expected 401" "$(head -c 300 "$body_file")"

# ----------------------------------------------------------- embeddings

section "Embeddings"

status="$(call POST /v1/embeddings '{"model":"embed-fast","input":"smoke test"}')"
if [ "$status" = "200" ]; then
  pass "embeddings work on embed-fast"
elif [ "$status" = "403" ]; then
  skip "embeddings" "the test key is not allowed to use embed-fast"
else
  fail "embeddings returned $status" "$(head -c 300 "$body_file")"
fi

# ---------------------------------------------------------------- spend

section "Spend accounting (acceptance 13)"

if [ -x "$GWCTL" ]; then
  # Spend is written asynchronously; give the proxy a moment.
  sleep 3
  spend="$("$GWCTL" spend --by consumer --since 1d --output json 2>/dev/null \
            | jq -r --arg consumer "$SMOKE_CONSUMER" '[.rows[] | select(.group == $consumer) | .cost_usd] | first // 0')"
  if awk "BEGIN {exit !($spend > 0)}"; then
    pass "gwctl spend reports a non-zero cost for ${SMOKE_CONSUMER} (${spend} USD)"
  else
    fail "gwctl spend reports nothing for ${SMOKE_CONSUMER}" "spend logging may be disabled"
  fi
else
  skip "gwctl spend" "$GWCTL is not built"
fi

# ----------------------------------------------------------------- logs

section "Log hygiene (acceptance 14)"

canary="canary-$(date +%s)-do-not-log-me"
call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_ALIAS" --arg canary "$canary" '{
  model: $model, max_tokens: 16, messages: [{role: "user", content: $canary}]
}')" >/dev/null

if command -v docker >/dev/null 2>&1 && $COMPOSE ps litellm >/dev/null 2>&1; then
  sleep 2
  if $COMPOSE logs --tail 500 litellm 2>/dev/null | grep -q "$canary"; then
    fail "the request body appears in the proxy logs" "logging.request_bodies must stay false"
  else
    pass "request bodies do not appear in the proxy logs"
  fi
else
  skip "log inspection" "docker compose is not available here"
fi

# ----------------------------------------------------- optional, gated

section "Optional checks"

if [ "${SMOKE_FALLBACK:-0}" = "1" ]; then
  status="$(call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_ALIAS" '{
    model: $model, max_tokens: 16, messages: [{role: "user", content: "hi"}]
  }')")"
  fallback="$(header_value x-gw-fallback)"
  if [ "$status" = "200" ] && [ "$fallback" = "true" ]; then
    pass "the request was served by the fallback provider (x-gw-fallback: true)"
  else
    fail "expected a fallback response" "status=$status x-gw-fallback=${fallback:-unset}"
  fi
else
  skip "fallback (acceptance 11)" "break the primary target of '${SMOKE_ALIAS}' and re-run with SMOKE_FALLBACK=1"
fi

if [ "${SMOKE_BUDGET:-0}" = "1" ]; then
  echo "  spending until the budget is exhausted…"
  status=200
  for _ in $(seq 1 200); do
    status="$(call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_ALIAS" '{
      model: $model, max_tokens: 256, messages: [{role: "user", content: "Write a long paragraph about clouds."}]
    }')")"
    [ "$status" = "429" ] && break
  done
  if [ "$status" = "429" ]; then
    pass "an exhausted budget returns 429"
    if jq -e '((.retry_after // .detail.retry_after) != null) and ((.budget // .detail.budget).limit_usd != null)' "$body_file" >/dev/null; then
      pass "the 429 body carries retry_after and budget details"
    else
      fail "the 429 body is not actionable" "$(head -c 300 "$body_file")"
    fi
  else
    fail "the budget was not exhausted after 200 requests" "last status: $status"
  fi
else
  skip "budget exhaustion (acceptance 8)" "re-run with SMOKE_BUDGET=1 and a consumer on a tiny budget"
fi

if [ "${SMOKE_DESTRUCTIVE:-0}" = "1" ] && [ -x "$GWCTL" ]; then
  new_key="$("$GWCTL" key rotate "$SMOKE_CONSUMER" --grace 24h --yes --output json 2>/dev/null | jq -r '.key // empty')"
  if [ -z "$new_key" ]; then
    fail "key rotation failed"
  else
    old_status="$(call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_ALIAS" '{
      model: $model, max_tokens: 16, messages: [{role: "user", content: "hi"}]
    }')")"
    GW_KEY="$new_key"
    new_status="$(call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_ALIAS" '{
      model: $model, max_tokens: 16, messages: [{role: "user", content: "hi"}]
    }')")"
    if [ "$old_status" = "200" ] && [ "$new_status" = "200" ]; then
      pass "after rotation with --grace both the old and the new key work (acceptance 15)"
    else
      fail "rotation broke one of the keys" "old=$old_status new=$new_status"
    fi

    "$GWCTL" key revoke "$SMOKE_CONSUMER" --grace 0 --yes >/dev/null 2>&1
    revoked_status="$(call POST /v1/chat/completions "$(jq -nc --arg model "$SMOKE_ALIAS" '{
      model: $model, max_tokens: 16, messages: [{role: "user", content: "hi"}]
    }')")"
    [ "$revoked_status" = "401" ] && pass "a revoked key is rejected with 401 (acceptance 7)" \
                                  || fail "a revoked key returned $revoked_status, expected 401"
  fi
else
  skip "revoke and rotate (acceptance 7, 15)" "re-run with SMOKE_DESTRUCTIVE=1; it replaces the consumer's key"
fi

# -------------------------------------------------------------- summary

printf '\n\033[1mSummary\033[0m\n'
printf '  passed  %d\n  failed  %d\n  skipped %d\n' "$passed" "$failed" "$skipped"
[ "$failed" -eq 0 ] || exit 1
