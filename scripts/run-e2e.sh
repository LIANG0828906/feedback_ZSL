#!/usr/bin/env bash
# End-to-end acceptance: hot/cold routing, overlap dedup, migration resume,
# connection cap (E_CONN), snapshot verification under load, load replay.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY || true

BIN=src/OrderRouter/bin/Release/net8.0/orderrouter
BASE=http://127.0.0.1:8298
ORDERS="${E2E_ORDERS:-20000}"
ITEMS="${E2E_ITEMS:-30000}"

echo "== build =="
dotnet build src/OrderRouter/OrderRouter.csproj -c Release --nologo -v q

echo "== init-db + seed ($ORDERS orders / $ITEMS items) =="
rm -f data/ledger.db data/ledger.db-wal data/ledger.db-shm
taskset -c 0,1 $BIN init-db
taskset -c 0,1 $BIN seed --orders "$ORDERS" --items "$ITEMS"

echo "== start service =="
taskset -c 0,1 $BIN serve > data/serve.log 2>&1 &
SERVE_PID=$!
trap 'kill $SERVE_PID 2>/dev/null || true' EXIT
for i in $(seq 1 30); do curl -sf "$BASE/status" > /dev/null && break; sleep 1; done
curl -s "$BASE/status" | python3 -m json.tool

echo "== routing: hot / cold / boundary =="
curl -s "$BASE/query?createdFrom=2026-09-01T00:00:00Z&createdTo=2026-09-02T00:00:00Z" | python3 -c "import json,sys;d=json.load(sys.stdin);print('hot   :',d['route'],d['tablesAccessed'],'split',d['splitCount'],'rows',d['rowCount'])"
curl -s "$BASE/query?createdFrom=2026-08-01T00:00:00Z&createdTo=2026-08-10T00:00:00Z" | python3 -c "import json,sys;d=json.load(sys.stdin);print('cold  :',d['route'],d['tablesAccessed'],'split',d['splitCount'],'rows',d['rowCount'])"
curl -s "$BASE/query?createdFrom=2026-08-20T00:00:00Z&createdTo=2026-08-22T00:00:00Z" | python3 -c "import json,sys;d=json.load(sys.stdin);print('bound :',d['route'],d['tablesAccessed'],'split',d['splitCount'],'rows',d['rowCount'])"

echo "== migration with concurrent writes + snapshot verification =="
taskset -c 0,1 $BIN migrate --batch-size 300 > data/migrate.log 2>&1 &
MIG_PID=$!
# drive reads + updates while migration runs
python3 - <<'PY'
import json, random, urllib.request
rng = random.Random(298)
base = 'http://127.0.0.1:8298/'
for i in range(300):
    oid = rng.randint(1, 20000)
    body = json.dumps({'orderId': oid, 'amount': round(rng.uniform(1, 1999), 2),
                       'currency': rng.choice(['USD','EUR','CNY','JPY'])}).encode()
    req = urllib.request.Request(base+'update', data=body, headers={'Content-Type':'application/json'})
    urllib.request.urlopen(req).read()
    urllib.request.urlopen(base+f'query?idFrom={max(1,oid-25)}&idTo={oid+25}').read()
print('drove 300 updates + 300 reads during migration')
PY
# snapshot consistency check while migration is (likely) still running
curl -s -X POST -d '' "$BASE/verify?count=500" | python3 -m json.tool
wait $MIG_PID
tail -2 data/migrate.log

echo "== ledger contiguity =="
python3 - <<'PY'
import sqlite3
c = sqlite3.connect('data/ledger.db')
rows = c.execute('SELECT batch_id,id_from_exclusive,id_to_inclusive,rows_copied FROM batches ORDER BY batch_id').fetchall()
prev = rows[0][1]
for b in rows:
    assert b[1] == prev, f'gap/overlap at {b}'
    prev = b[2]
print(f'ledger OK: {len(rows)} contiguous ranges, {sum(r[3] for r in rows)} rows copied')
print('meta:', c.execute('SELECT * FROM meta').fetchall())
PY

echo "== interrupt + resume (kill -9 mid-run, then continue) =="
rm -f data/ledger.db data/ledger.db-wal data/ledger.db-shm
taskset -c 0,1 $BIN init-db
taskset -c 0,1 $BIN seed --orders "$ORDERS" --items "$ITEMS"
taskset -c 0,1 $BIN migrate --batch-size 150 --batch-delay-ms 150 > data/migrate1.log 2>&1 &
MIG_PID=$!
sleep 2
kill -9 $MIG_PID 2>/dev/null || true
wait $MIG_PID 2>/dev/null || true
BEFORE=$(python3 -c "import sqlite3;print(sqlite3.connect('data/ledger.db').execute('SELECT COUNT(*) FROM batches').fetchone()[0])")
echo "committed batches before kill-resume: $BEFORE"
taskset -c 0,1 $BIN migrate --batch-size 150 > data/migrate2.log 2>&1
python3 - <<'PY'
import sqlite3
c = sqlite3.connect('data/ledger.db')
rows = c.execute('SELECT batch_id,id_from_exclusive,id_to_inclusive FROM batches ORDER BY batch_id').fetchall()
prev = rows[0][1]
for b in rows:
    assert b[1] == prev, f'gap/overlap at {b}'
    prev = b[2]
ids = [r[0] for r in rows]
assert ids == list(range(1, len(ids)+1)), 'batch ids not contiguous'
print(f'resume OK: {len(rows)} contiguous ranges after kill -9, no duplicates, no gaps')
PY

echo "== post-migration routing + overlap dedup =="
curl -s "$BASE/query?createdFrom=2026-08-01T00:00:00Z&createdTo=2026-08-10T00:00:00Z" | python3 -c "import json,sys;d=json.load(sys.stdin);print('cold after complete:',d['route'],d['tablesAccessed'],'rows',d['rowCount'])"
# update a migrated order twice, expect convergence to the newest value
OID=$(curl -s "$BASE/query?createdFrom=2026-07-25T00:00:00Z&createdTo=2026-08-01T00:00:00Z" | python3 -c "import json,sys;print(json.load(sys.stdin)['rows'][0]['orderId'])")
curl -s -X POST -d "{\"orderId\":$OID,\"amount\":111.11,\"currency\":\"EUR\"}" -H 'Content-Type: application/json' "$BASE/update" > /dev/null
curl -s -X POST -d "{\"orderId\":$OID,\"amount\":222.22,\"currency\":\"JPY\"}" -H 'Content-Type: application/json' "$BASE/update" > /dev/null
curl -s "$BASE/query?idFrom=$OID&idTo=$OID" | python3 -c "
import json,sys
d=json.load(sys.stdin)
r=d['rows'][0]
assert r['amount']==222.22 and r['currency']=='JPY', r
assert d['dedupDropped']==1, d
print('overlap dedup + convergence OK:', r['orderId'], r['amount'], r['currency'], 'source', r['source'])"

echo "== connection cap / E_CONN =="
PIDS=""
for i in $(seq 1 8); do curl -s "$BASE/debug/slowquery?ms=4000" > /dev/null & PIDS="$PIDS $!"; done
sleep 1
curl -s "$BASE/connections"
echo
curl -s "$BASE/query?idFrom=1&idTo=10" | python3 -c "import json,sys;d=json.load(sys.stdin);assert d.get('errorCode')=='E_CONN',d;print('E_CONN OK:',d['errorCode'])"
wait $PIDS

echo "== short-query P95 under batch-slot saturation =="
PIDS=""
for i in $(seq 1 5); do curl -s "$BASE/debug/slowquery?ms=3000&class=batch" > /dev/null & PIDS="$PIDS $!"; done
sleep 1
python3 - <<'PY'
import time, urllib.request, json
lats=[]
for i in range(20):
    t0=time.perf_counter()
    urllib.request.urlopen(f'http://127.0.0.1:8298/query?idFrom={i*10+1}&idTo={i*10+5}').read()
    lats.append((time.perf_counter()-t0)*1000)
lats.sort()
p95=lats[int(len(lats)*0.95)-1]
print(f'interactive P95 under saturated batch slots: {p95:.1f} ms (limit 300 ms)')
assert p95 < 300, 'P95 exceeded 300ms'
PY
wait $PIDS

echo "== load replay =="
taskset -c 0,1 $BIN replay | tail -5

echo "== metrics =="
curl -s "$BASE/metrics"

echo "== final status =="
curl -s "$BASE/status" | python3 -m json.tool

echo "E2E OK"
