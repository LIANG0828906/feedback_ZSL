#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
GROUP="${GROUP:-smoke-$(date +%s)}"

curl -fsS -X POST "$BASE_URL/api/subscriptions" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"$GROUP\",\"eventTypes\":[\"ORDER_CREATED\",\"ORDER_PAID\",\"ORDER_SHIPPED\",\"REFUND_REQUESTED\",\"RETURN_REQUESTED\"],\"conditions\":[]}" \
  >/dev/null

PULL="$(curl -fsS -X POST "$BASE_URL/api/subscriptions/$GROUP/pull" \
  -H 'Content-Type: application/json' \
  -d '{"partitions":[0],"maxEventsPerPartition":3}')"

python3 - "$BASE_URL" "$GROUP" "$PULL" <<'PY'
import json
import sys
import urllib.request

base, group, raw = sys.argv[1], sys.argv[2], sys.argv[3]
pull = json.loads(raw)
part = pull["partitions"][0]
events = part["events"]
if not events:
    raise SystemExit("expected at least one matching event on partition 0")

def post(path, body):
    request = urllib.request.Request(
        base + path,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.load(error)

first = events[0]["offset"]
if len(events) > 1:
    code, body = post(f"/api/subscriptions/{group}/confirmations", {
        "partition": 0, "offset": events[-1]["offset"]
    })
    if code != 409 or body["code"] != "OFFSET_OUT_OF_ORDER":
        raise SystemExit(f"out-of-order confirmation was not rejected: {code} {body}")

for event in events:
    code, body = post(f"/api/subscriptions/{group}/confirmations", {
        "partition": 0, "offset": event["offset"]
    })
    if code != 200:
        raise SystemExit(f"confirmation failed: {code} {body}")

status = json.load(urllib.request.urlopen(
    f"{base}/api/subscriptions/{group}/status", timeout=10))
p0 = status["partitions"][0]
if p0["confirmedOffset"] != events[-1]["offset"]:
    raise SystemExit(f"confirmed offset mismatch: {p0}")

attribution = json.load(urllib.request.urlopen(
    f"{base}/api/subscriptions/{group}/attribution", timeout=30))
if not attribution["totals"]["balanced"]:
    raise SystemExit(f"attribution does not reconcile: {attribution['totals']}")

print(json.dumps({
    "group": group,
    "firstOffset": first,
    "confirmedOffset": p0["confirmedOffset"],
    "attribution": attribution["totals"],
}, indent=2))
PY

echo "smoke test passed for $GROUP"
