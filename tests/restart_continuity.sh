#!/usr/bin/env bash
# Simulator restart continuity + collector restart idempotency check.
set -euo pipefail
cd "$(dirname "$0")/.."

python3 - <<'PY'
import os, shutil
for p in ("snmpmon.db", "snmpmon.db-wal", "snmpmon.db-shm"):
    if os.path.exists(p):
        os.remove(p)
if os.path.isdir("sim_state"):
    shutil.rmtree("sim_state")
os.makedirs("sim_state")
PY

python3 -m snmpmon.simulator --config config.json &
SIM_PID=$!
sleep 1.5
python3 -m snmpmon.collector --config config.json --cycles 5 > /dev/null 2>&1
python3 - <<'PY'
import sqlite3
c = sqlite3.connect("snmpmon.db")
q = "SELECT value FROM samples WHERE device='sw1' AND oid=? ORDER BY cycle"
print("before restart uptime:", [r[0] for r in c.execute(q, ("1.3.6.1.2.1.1.3.0",))])
print("before restart ifInOctets.1:", [r[0] for r in c.execute(q, ("1.3.6.1.2.1.2.2.1.10.1",))])
PY
kill "$SIM_PID"
wait "$SIM_PID" 2>/dev/null || true
sleep 0.5

python3 -m snmpmon.simulator --config config.json &
SIM_PID=$!
sleep 1.5
python3 -m snmpmon.collector --config config.json --cycles 5 > /dev/null 2>&1
python3 - <<'PY'
import sqlite3
c = sqlite3.connect("snmpmon.db")
q = "SELECT value FROM samples WHERE device='sw1' AND oid=? ORDER BY cycle"
print("after restart uptime:", [r[0] for r in c.execute(q, ("1.3.6.1.2.1.1.3.0",))])
print("after restart ifInOctets.1:", [r[0] for r in c.execute(q, ("1.3.6.1.2.1.2.2.1.10.1",))])
print("total sw1 cycles:", c.execute("SELECT COUNT(*) FROM device_cycles WHERE device='sw1'").fetchone()[0])
print("duplicate samples:", c.execute(
    "SELECT COUNT(*) FROM (SELECT device, cycle, oid FROM samples "
    "GROUP BY 1, 2, 3 HAVING COUNT(*) > 1)").fetchone()[0])
PY
kill "$SIM_PID"
wait "$SIM_PID" 2>/dev/null || true
