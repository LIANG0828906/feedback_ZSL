#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
GO_BIN="${GO_BIN:-go}"
ETCD_BIN="${ETCD_BIN:-$ROOT/.run/etcd-linux/etcd}"
FAULT=0
[[ "${1:-}" == "--fault" ]] && FAULT=1

mkdir -p .run/bin .run/acceptance-logs artifacts data
if [[ ! -x "$ETCD_BIN" ]]; then
  echo "etcd Linux binary not found at $ETCD_BIN; set ETCD_BIN" >&2
  exit 2
fi

echo "building Linux acceptance binaries"
GOTOOLCHAIN=local CGO_ENABLED=0 "$GO_BIN" build -trimpath -o .run/bin/coordinator ./cmd/coordinator
GOTOOLCHAIN=local CGO_ENABLED=0 "$GO_BIN" build -trimpath -o .run/bin/worker ./cmd/worker
GOTOOLCHAIN=local CGO_ENABLED=0 "$GO_BIN" build -trimpath -o .run/bin/taskgen ./cmd/taskgen
GOTOOLCHAIN=local CGO_ENABLED=0 "$GO_BIN" build -trimpath -o .run/bin/probe ./cmd/probe
GOTOOLCHAIN=local CGO_ENABLED=0 "$GO_BIN" build -trimpath -o .run/bin/verify ./cmd/verify

find data -maxdepth 1 -type f -name 'acceptance.db*' -delete
ETCD_PID=0
if curl -fsS http://127.0.0.1:2379/health >/dev/null 2>&1; then
  echo "reusing etcd on 127.0.0.1:2379"
else
  mkdir -p .run/acceptance-etcd
  find .run/acceptance-etcd -mindepth 1 -delete
  "$ETCD_BIN" --name acceptance --data-dir .run/acceptance-etcd \
    --listen-client-urls http://127.0.0.1:2379 \
    --advertise-client-urls http://127.0.0.1:2379 \
    --listen-peer-urls http://127.0.0.1:2380 \
    --initial-advertise-peer-urls http://127.0.0.1:2380 \
    --initial-cluster acceptance=http://127.0.0.1:2380 \
    --initial-cluster-state new >.run/acceptance-logs/etcd.log 2>&1 &
  ETCD_PID=$!
fi
COORD_PID=""
WORKER_PIDS=()
cleanup() {
  for pid in "${WORKER_PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  [[ -n "$COORD_PID" ]] && kill "$COORD_PID" 2>/dev/null || true
  [[ "$ETCD_PID" != 0 ]] && kill "$ETCD_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

for _ in $(seq 1 100); do curl -fsS http://127.0.0.1:2379/health >/dev/null 2>&1 && break; sleep 0.1; done

python3 - <<'PY'
import json, uuid
cfg=json.load(open('configs/coordinator.json'))
cfg['key_prefix']='/settlement/probe-'+uuid.uuid4().hex
json.dump(cfg,open('.run/acceptance-logs/coordinator-probe.json','w'),indent=2)
PY
GOMAXPROCS=1 .run/bin/coordinator -config .run/acceptance-logs/coordinator-probe.json >.run/acceptance-logs/coordinator-probe.log 2>&1 &
COORD_PID=$!
for _ in $(seq 1 100); do curl -fsS http://127.0.0.1:19090/healthz >/dev/null 2>&1 && break; sleep 0.1; done
.run/bin/probe -grpc 127.0.0.1:17070 -out artifacts/lock-probe-report.json >.run/acceptance-logs/probe.out
kill "$COORD_PID" 2>/dev/null || true
wait "$COORD_PID" 2>/dev/null || true
COORD_PID=""
sleep 0.3

python3 - <<'PY'
import json, uuid
cfg=json.load(open('configs/coordinator.json'))
cfg['key_prefix']='/settlement/acceptance-'+uuid.uuid4().hex
json.dump(cfg,open('.run/acceptance-logs/coordinator-scenario.json','w'),indent=2)
PY
GOMAXPROCS=1 .run/bin/coordinator -config .run/acceptance-logs/coordinator-scenario.json >.run/acceptance-logs/coordinator.log 2>&1 &
COORD_PID=$!
for _ in $(seq 1 100); do curl -fsS http://127.0.0.1:19090/healthz >/dev/null 2>&1 && break; sleep 0.1; done

WORK_MS=250
[[ "$FAULT" == 1 ]] && WORK_MS=1200
python3 - "$WORK_MS" <<'PY'
import json, os, sys
root=os.getcwd(); work=int(sys.argv[1])
for w in 'ABC':
    cfg={
      'worker_id':w,'grpc_address':'127.0.0.1:17070',
      'database_path':os.path.join(root,'data','acceptance.db'),
      'tasks_file':os.path.join(root,'data','tasks.jsonl'),
      'seed':28120261119,'batch_count':20,'tasks_per_batch':100,
      'lease_ttl_ms':6000,'wait_timeout_ms':1500,'renew_interval_ms':2000,
      'batch_work_ms':work,'retry_delay_ms':150,'max_runtime_ms':110000,
    }
    json.dump(cfg,open(f'.run/acceptance-logs/worker-{w}.json','w'),indent=2)
PY
started=$(date +%s)
GOMAXPROCS=1 .run/bin/worker -config .run/acceptance-logs/worker-A.json >.run/acceptance-logs/worker-A.log 2>&1 & WORKER_PIDS+=($!)
GOMAXPROCS=1 .run/bin/worker -config .run/acceptance-logs/worker-B.json >.run/acceptance-logs/worker-B.log 2>&1 & B_PID=$!; WORKER_PIDS+=($B_PID)
GOMAXPROCS=1 .run/bin/worker -config .run/acceptance-logs/worker-C.json >.run/acceptance-logs/worker-C.log 2>&1 & WORKER_PIDS+=($!)

fault_triggered=false
if [[ "$FAULT" == 1 ]]; then
  for _ in $(seq 1 600); do
    batch=$(curl -fsS http://127.0.0.1:19090/v1/status 2>/dev/null | python3 -c 'import json,sys
try:
 d=json.load(sys.stdin)
 xs=[x["batch_id"] for x in d.get("statuses",[]) if x.get("held") and x.get("holder_id")=="B" and not x.get("committed")]
 print(xs[0] if xs else "")
except Exception: print("")' || true)
    if [[ -n "$batch" ]]; then
      echo "injecting fault: killing B while holding $batch"
      kill -9 "$B_PID" 2>/dev/null || true
      fault_triggered=true
      break
    fi
    sleep 0.05
  done
  [[ "$fault_triggered" == true ]] || { echo 'B never held an uncommitted batch' >&2; exit 3; }
  sleep 7.5
  GOMAXPROCS=1 .run/bin/worker -config .run/acceptance-logs/worker-B.json >.run/acceptance-logs/worker-B-restart.log 2>&1 & B_PID=$!; WORKER_PIDS+=($B_PID)
fi

for _ in $(seq 1 1100); do
  alive=0
  for pid in "${WORKER_PIDS[@]}"; do kill -0 "$pid" 2>/dev/null && alive=$((alive+1)); done
  [[ "$alive" -eq 0 ]] && break
  sleep 0.1
done
elapsed=$(( $(date +%s) - started ))
[[ "$alive" -eq 0 ]] || { echo 'workers timed out' >&2; exit 4; }

.run/bin/verify -grpc 127.0.0.1:17070 -db data/acceptance.db -out artifacts/acceptance-report.json >.run/acceptance-logs/verify.out
curl -fsS http://127.0.0.1:19090/metrics > artifacts/acceptance-metrics.txt
curl -fsS http://127.0.0.1:19090/v1/status > artifacts/acceptance-status.json

python3 - "$FAULT" "$elapsed" "$fault_triggered" <<'PY'
import json, sys
fault=bool(int(sys.argv[1])); elapsed=int(sys.argv[2]); triggered=sys.argv[3]=='true'
r=json.load(open('artifacts/acceptance-report.json'))
status=json.load(open('artifacts/acceptance-status.json'))
metrics=open('artifacts/acceptance-metrics.txt').read()
errors=[]
if not r.get('ok'): errors.append('verification report is not ok')
if r.get('committed_batches')!=20 or r.get('persisted_batches')!=20: errors.append('expected 20 batches')
if r.get('settlement_task_rows')!=2000: errors.append('expected 2000 task rows')
if len(status.get('statuses',[]))!=20: errors.append('status did not return 20 batches')
if elapsed>=120: errors.append('scenario exceeded 120 seconds')
if fault and (not triggered or r.get('lease_expired_releases',0)<1): errors.append('fault release event missing')
for needle in ('settlement_lock_held{batch="batch-001"}',
               'settlement_lock_owner_info{batch="batch-001",holder=',
               'settlement_lock_lease_remaining_seconds{batch="batch-001"}',
               'settlement_lock_acquire_requests_total{batch="batch-001"}',
               'settlement_lock_renew_requests_total{batch="batch-001"}',
               'settlement_lock_release_events_total{batch="batch-001",reason=',
               'settlement_lock_wait_timeouts_total{batch="batch-001"}'):
    if needle not in metrics: errors.append('missing metric '+needle)
summary={
 'ok':not errors,'errors':errors,'fault_enabled':fault,'fault_triggered':triggered,
 'scenario_seconds':elapsed,'committed_batches':r.get('committed_batches'),
 'persisted_batches':r.get('persisted_batches'),'settlement_task_rows':r.get('settlement_task_rows'),
 'release_events':r.get('release_events'),'lease_expired_releases':r.get('lease_expired_releases')}
open('artifacts/acceptance-summary.json','w').write(json.dumps(summary,indent=2)+'\n')
print(json.dumps(summary,indent=2))
if errors: sys.exit(1)
PY
