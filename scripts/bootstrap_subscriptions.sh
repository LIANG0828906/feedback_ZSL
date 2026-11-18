#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"

create_group() {
  local name="$1"
  local body="$2"
  local response
  local code
  response="$(mktemp)"
  code="$(curl -sS -o "$response" -w '%{http_code}' \
    -X POST "$BASE_URL/api/subscriptions" \
    -H 'Content-Type: application/json' \
    -d "$body")"
  if [[ "$code" == "201" ]]; then
    echo "created $name"
  elif [[ "$code" == "409" ]]; then
    echo "already exists $name"
  else
    echo "failed to create $name: HTTP $code $(cat "$response")" >&2
    return 1
  fi
}

for _ in $(seq 1 30); do
  if curl -fsS "$BASE_URL/health" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

create_group notification \
  '{"name":"notification","rule":{"eventTypes":["ORDER_PAID","ORDER_SHIPPED"],"conditions":[]}}'
create_group risk-control \
  '{"name":"risk-control","rule":{"eventTypes":["ORDER_CREATED","REFUND_REQUESTED"],"conditions":[]}}'
create_group after-sales \
  '{"name":"after-sales","rule":{"eventTypes":["REFUND_REQUESTED","RETURN_REQUESTED"],"conditions":[]}}'
