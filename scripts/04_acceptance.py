#!/usr/bin/env python3
"""End-to-end acceptance checks for slotguard.

The script is intentionally self-contained: it seeds the schema if needed,
bootstraps a missing/broken subscription, injects consumer-position drift and
table gaps, runs a live workload, and kills a resync process mid-copy to test
durable progress.

It only uses the configured primary/replica and the prefixed Redis key.
"""
from __future__ import annotations

import json
import re
import socket
import sqlite3
import subprocess
import sys
import time
import urllib.request
import uuid
from dataclasses import replace
from pathlib import Path
from typing import Any, Callable

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "pylibs"))
sys.path.insert(0, str(ROOT / "src"))

import psycopg  # noqa: E402
import redis  # noqa: E402
from psycopg import sql  # noqa: E402

from config import load_config  # noqa: E402
from db import connect  # noqa: E402
from metrics import start_metrics_server  # noqa: E402
from resync import ResyncManager  # noqa: E402
from service import SlotGuardService  # noqa: E402
from slots import collect  # noqa: E402
from store import Store  # noqa: E402

PASS, FAIL = "PASS", "FAIL"
results: list[tuple[str, str, str]] = []


def check(name: str, ok: bool, detail: str = "") -> None:
    results.append((name, PASS if ok else FAIL, detail))
    print(f"[{PASS if ok else FAIL}] {name} {detail}", flush=True)


def run_py(*args: str, capture: bool = False) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, *args],
        cwd=ROOT,
        check=True,
        text=True,
        capture_output=capture,
    )


def scalar(dsn: str, statement: sql.Composable | str, params=()) -> Any:
    with connect(dsn, 5, autocommit=True) as conn, conn.cursor() as cur:
        cur.execute(statement, params)
        row = cur.fetchone()
        return row[0] if row else None


def table_counts(cfg) -> tuple[int, int, int, int, int]:
    with connect(cfg.primary_dsn, 5, autocommit=True) as p, \
            connect(cfg.replica_dsn, 5, autocommit=True) as r, \
            p.cursor() as pc, r.cursor() as rc:
        pc.execute(
            sql.SQL("SELECT count(*) FROM {}").format(
                sql.Identifier(*cfg.parent_table.split("."))
            )
        )
        po = pc.fetchone()[0]
        pc.execute(
            sql.SQL("SELECT count(*) FROM {}").format(
                sql.Identifier(*cfg.child_table.split("."))
            )
        )
        pd = pc.fetchone()[0]
        rc.execute(
            sql.SQL("SELECT count(*) FROM {}").format(
                sql.Identifier(*cfg.parent_table.split("."))
            )
        )
        ro = rc.fetchone()[0]
        rc.execute(
            sql.SQL("SELECT count(*) FROM {}").format(
                sql.Identifier(*cfg.child_table.split("."))
            )
        )
        rd = rc.fetchone()[0]
        rc.execute(
            sql.SQL(
                "SELECT count(*) FROM {child} d WHERE NOT EXISTS ("
                "SELECT 1 FROM {parent} p WHERE p.{pk}=d.{fk})"
            ).format(
                child=sql.Identifier(*cfg.child_table.split(".")),
                parent=sql.Identifier(*cfg.parent_table.split(".")),
                pk=sql.Identifier(cfg.parent_key),
                fk=sql.Identifier(cfg.child_fk),
            )
        )
        orphan = rc.fetchone()[0]
    return po, ro, pd, rd, orphan


def wait_for(predicate: Callable[[], bool], timeout: float = 20.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return True
        time.sleep(0.1)
    return False


def local_latest_progress(path: str) -> tuple[str | None, int]:
    db = sqlite3.connect(path, timeout=5)
    try:
        row = db.execute(
            "SELECT job_id FROM resync_jobs ORDER BY created_at DESC LIMIT 1"
        ).fetchone()
        if not row:
            return None, 0
        job_id = row[0]
        keys = [
            int(value) for (value,) in db.execute(
                "SELECT last_key FROM resync_table_progress "
                "WHERE job_id=? AND last_key IS NOT NULL",
                (job_id,),
            ).fetchall()
        ]
        return job_id, max(keys, default=0)
    finally:
        db.close()


def main() -> int:
    cfg = load_config()
    try:
        table_counts(cfg)
    except psycopg.Error:
        run_py("scripts/01_init_schema_seed.py", capture=True)

    store = Store(cfg.sqlite_path)
    svc = SlotGuardService(cfg, store)
    manager = ResyncManager(cfg, store)
    metrics_server = None

    try:
        # A subscription that does not exist, or a replica with a gap, is
        # healed before the checks begin.
        po, ro, pd, rd, orphan = table_counts(cfg)
        sub_exists = scalar(
            cfg.replica_dsn,
            "SELECT count(*) FROM pg_subscription WHERE subname=%s",
            (cfg.subscription_name,),
        )
        slot_exists = scalar(
            cfg.primary_dsn,
            "SELECT count(*) FROM pg_replication_slots WHERE slot_name=%s",
            (cfg.slot_name,),
        )
        if not sub_exists or not slot_exists or (po, pd) != (ro, rd) or orphan:
            outcome = manager.run("acceptance-bootstrap")
            check(
                "bootstrap-subscription",
                outcome["status"] == "done",
                str(outcome.get("verify", {})),
            )

        # 1. Unified view and per-instance failure isolation.
        monitor = svc.monitor_once()
        check(
            "unified-slot-view",
            set(monitor["slots"]["per_instance"]) == {"primary", "replica"},
            f"slots={sorted(monitor['slots']['merged'])}",
        )
        dual_slot = f"acc_dual_{uuid.uuid4().hex[:8]}"
        created_slots: list[str] = []
        try:
            for dsn in (cfg.primary_dsn, cfg.replica_dsn):
                with connect(dsn, 5, autocommit=True) as conn:
                    conn.execute(
                        "SELECT pg_create_logical_replication_slot(%s, 'pgoutput')",
                        (dual_slot,),
                    )
                created_slots.append(dsn)
            dual_view = collect(cfg)["merged"].get(dual_slot, {})
            check(
                "same-slot-both-instances-marked",
                dual_view.get("exists_on_both") is True
                and set(dual_view.get("instances", {})) == {"primary", "replica"},
            )
        finally:
            for dsn in created_slots:
                with connect(dsn, 5, autocommit=True) as conn:
                    conn.execute(
                        "SELECT pg_drop_replication_slot(%s)", (dual_slot,)
                    )
        bad_cfg = replace(
            cfg, replica_dsn="host=127.0.0.1 port=1 dbname=postgres "
                             "connect_timeout=1"
        )
        isolated = collect(bad_cfg)
        replica_errors = isolated["per_instance"]["replica"]["errors"]
        check(
            "instance-error-isolated",
            any(e["code"] == "SG-E1001" for e in replica_errors)
            and bool(isolated["per_instance"]["primary"]["slots"]),
            str(replica_errors[:1]),
        )

        # 2. Tiered relief and automatic revocation.
        run_py("scripts/03_fault_inject.py", "disconnect", capture=True)
        old_threshold = cfg.wal_retention_threshold_bytes
        cfg.wal_retention_threshold_bytes = 16
        levels: list[str] = []
        for _ in range(3):
            result = svc.monitor_once()
            levels.extend(
                a["level"] for a in result["advices"]
                if a["slot_name"] == cfg.slot_name
            )
            time.sleep(0.05)
        pending = [
            dict(row) for row in store.list_pending_advices()
            if row["slot_name"] == cfg.slot_name
        ]
        check(
            "relief-tiered",
            "notify_catchup" in levels
            and any(p["level"] == "recycle_candidate" for p in pending),
            str([(p["level"], p["recheck_count"]) for p in pending]),
        )
        check(
            "relief-retains-audit-fields",
            bool(pending and pending[0]["confirmed_lsn"]
                 and pending[0]["subscriber_id"]),
            str(pending[0]["confirmed_lsn"] if pending else None),
        )
        with connect(cfg.replica_dsn, 5, autocommit=True) as conn:
            conn.execute(
                sql.SQL("ALTER SUBSCRIPTION {} ENABLE").format(
                    sql.Identifier(cfg.subscription_name)
                )
            )
        time.sleep(1)
        svc.monitor_once()
        check(
            "relief-auto-revoked",
            not any(
                row["slot_name"] == cfg.slot_name
                for row in store.list_pending_advices()
            ),
        )
        cfg.wal_retention_threshold_bytes = old_threshold

        # 3. Redis position reconciliation.
        sentinel = f"{cfg.redis_key_prefix}acceptance-sentinel"
        redis_client = redis.Redis.from_url(cfg.redis_url)
        redis_client.set(sentinel, "keep")
        svc.reconciler.delete_offset()
        missing = svc.reconcile_once()
        ahead = svc.reconciler.tamper(delta=0x10000000)
        ahead_result = svc.reconcile_once()
        with connect(cfg.primary_dsn, 5) as conn, conn.cursor() as cur:
            cur.execute(
                sql.SQL(
                    "INSERT INTO {} (order_no, warehouse, status, total_amount) "
                    "VALUES (%s,'WH-EAST','CREATED',0)"
                ).format(sql.Identifier(*cfg.parent_table.split("."))),
                (f"SO-ACC-{uuid.uuid4().hex[:10]}",),
            )
            conn.commit()
        time.sleep(2)
        svc.reconciler.tamper(delta=-0x100000)
        behind = svc.reconcile_once()
        equal = svc.reconcile_once()
        check(
            "offset-missing-initialized",
            missing["verdict"] == "missing"
            and missing["corrected_lsn"] == missing["applied_lsn"],
            missing["source_of_truth"],
        )
        check(
            "offset-ahead-rejected",
            ahead_result["verdict"] == "ahead_rejected"
            and ahead_result["corrected_lsn"] == ahead_result["applied_lsn"],
            str(ahead),
        )
        check(
            "offset-behind-corrected",
            behind["verdict"] == "behind"
            and equal["verdict"] == "equal",
            f"{behind['redis_lsn']}->{behind['corrected_lsn']}",
        )
        check(
            "offset-preserves-other-keys",
            redis_client.get(sentinel) == b"keep",
        )
        redis_client.close()

        # 4. Read-only sampling detects drift and recovers.
        with connect(cfg.primary_dsn, 5, autocommit=True) as conn, \
                conn.cursor() as cur:
            cur.execute(
                sql.SQL(
                    "SELECT {key}, status FROM {table} "
                    "ORDER BY hashtextextended({key}::text, %s) LIMIT 1"
                ).format(
                    key=sql.Identifier(cfg.parent_key),
                    table=sql.Identifier(*cfg.parent_table.split(".")),
                ),
                (cfg.sample_seed,),
            )
            sampled_key, good_status = cur.fetchone()
        with connect(cfg.replica_dsn, 5) as conn, conn.cursor() as cur:
            cur.execute(
                sql.SQL("UPDATE {} SET status=%s WHERE {}={}").format(
                    sql.Identifier(*cfg.parent_table.split(".")),
                    sql.Identifier(cfg.parent_key),
                    sql.Placeholder(),
                ),
                ("ACCEPTANCE-DRIFT", sampled_key),
            )
            conn.commit()
        drift = svc.sampling_once()
        found = any(
            m["primary_key"] == f"{cfg.parent_key}={sampled_key}"
            and m["column_name"] == "status"
            for m in drift["mismatches"]
        )
        with connect(cfg.replica_dsn, 5) as conn, conn.cursor() as cur:
            cur.execute(
                sql.SQL("UPDATE {} SET status=%s WHERE {}={}").format(
                    sql.Identifier(*cfg.parent_table.split(".")),
                    sql.Identifier(cfg.parent_key),
                    sql.Placeholder(),
                ),
                (good_status, sampled_key),
            )
            conn.commit()
        clean = svc.sampling_once()
        check(
            "sampling-detects-drift",
            found and drift["orders_sampled"] == cfg.sample_order_count,
            str(drift["mismatches"][:1]),
        )
        check("sampling-clean-after-repair", not clean["mismatches"])

        # 5. Slot deletion + gap + a live 50 TPS workload.
        run_py("scripts/03_fault_inject.py", "drop-slot", capture=True)
        run_py("scripts/03_fault_inject.py", "gap", capture=True)
        workload = subprocess.Popen(
            [
                sys.executable, str(ROOT / "scripts" / "02_workload.py"),
                "--duration", "16", "--rate", "50",
            ],
            cwd=ROOT, text=True, stdout=subprocess.PIPE,
        )
        outcome = manager.run("acceptance-concurrent")
        workload_out, _ = workload.communicate(timeout=40)
        verify = outcome["verify"]
        p95_match = re.search(r"p95=([0-9.]+)ms", workload_out or "")
        p95 = float(p95_match.group(1)) if p95_match else 999.0
        check(
            "resync-under-live-writes",
            outcome["status"] == "done" and verify["consistent"]
            and verify["orphan_details"] == 0,
            f"orphans={verify['orphan_details']}",
        )
        check("business-write-p95-under-50ms", p95 <= 50.0, f"p95={p95}ms")

        # 6. Hard-kill a resync child after a committed batch, then resume.
        run_py("scripts/03_fault_inject.py", "drop-slot", capture=True)
        run_py("scripts/03_fault_inject.py", "gap", capture=True)
        workload = subprocess.Popen(
            [
                sys.executable, str(ROOT / "scripts" / "02_workload.py"),
                "--duration", "18", "--rate", "50",
            ],
            cwd=ROOT, text=True, stdout=subprocess.PIPE,
        )
        previous_job, _ = local_latest_progress(cfg.sqlite_path)
        child = subprocess.Popen(
            [sys.executable, str(ROOT / "src" / "cli.py"), "resync",
             "--reason", "acceptance-kill-resume"],
            cwd=ROOT, text=True, stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        wait_for(
            lambda: (
                local_latest_progress(cfg.sqlite_path)[0] != previous_job
                and local_latest_progress(cfg.sqlite_path)[1] >= 5000
            ),
            timeout=15,
        )
        child.kill()
        child.wait(timeout=10)
        killed_job, before_key = local_latest_progress(cfg.sqlite_path)
        resumed = manager.run("acceptance-kill-resume")
        workload_out, _ = workload.communicate(timeout=45)
        verify = resumed["verify"]
        check(
            "resync-resumes-after-kill",
            resumed["status"] == "done" and verify["consistent"]
            and before_key > 0,
            f"job={killed_job} before_key={before_key}",
        )
        check(
            "resync-final-counts-match",
            all(
                value["count_match"]
                for value in verify.values() if isinstance(value, dict)
            ) and verify["orphan_details"] == 0,
            str(verify),
        )

        # 7. Repeating a completed resync must not duplicate primary keys.
        repeated = manager.run("acceptance-repeat")
        with connect(cfg.primary_dsn, 5, autocommit=True) as p, \
                connect(cfg.replica_dsn, 5, autocommit=True) as r:
            unique_ok = True
            for table, key in (
                (cfg.parent_table, cfg.parent_key),
                (cfg.child_table, cfg.child_key),
            ):
                ident = sql.Identifier(*table.split("."))
                q = sql.SQL(
                    "SELECT count(*)=count(DISTINCT {}) FROM {}"
                ).format(sql.Identifier(key), ident)
                unique_ok = unique_ok and bool(
                    p.execute(q).fetchone()[0]
                ) and bool(r.execute(q).fetchone()[0])
        check(
            "resync-repeat-no-duplicates",
            repeated["status"] == "done" and unique_ok,
        )

        # 8. Incomplete round recovery and immutable completed rounds.
        resume_round = f"smp-{uuid.uuid4().hex}"
        store.begin_round(resume_round, "sampling", time.time())
        recovered = svc.recover_incomplete_rounds()
        check(
            "incomplete-round-resumed",
            any(item["round_id"] == resume_round for item in recovered)
            and store.round_status(resume_round) == "done",
        )
        before_mismatches = store.get_counter("sample_mismatches_total")
        skipped = svc.sampling_once(resume_round)
        check(
            "completed-round-immutable",
            skipped.get("skipped") is not None
            and store.get_counter("sample_mismatches_total") == before_mismatches,
        )

        # 9. Stable Prometheus names and per-instance labels.
        try:
            socket.create_connection(
                (cfg.metrics_host, cfg.metrics_port), timeout=0.5
            ).close()
        except OSError:
            metrics_server = start_metrics_server(cfg, store)
            time.sleep(0.2)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        text = opener.open(
            f"http://{cfg.metrics_host}:{cfg.metrics_port}/metrics", timeout=5
        ).read().decode()
        required = {
            "slotguard_replication_slots",
            "slotguard_inactive_replication_slots",
            "slotguard_slot_lag_bytes_max",
            "slotguard_resync_tables_in_progress",
            "slotguard_offset_corrections_total",
            "slotguard_sample_mismatches_total",
        }
        names = {
            line.split("{")[0].split(" ")[0]
            for line in text.splitlines()
            if line and not line.startswith("#")
        }
        check(
            "prometheus-stable-metrics",
            required.issubset(names)
            and 'instance="primary"' in text
            and 'instance="replica"' in text,
        )

        started = time.perf_counter()
        svc.monitor_once()
        elapsed = time.perf_counter() - started
        check(
            "monitor-refresh-under-15s",
            elapsed <= 15,
            f"{elapsed:.3f}s",
        )
    finally:
        if metrics_server is not None:
            metrics_server.shutdown()
        svc.close()
        store.close()

    failed = [row for row in results if row[1] == FAIL]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed")
    if failed:
        print(json.dumps(failed, ensure_ascii=False, indent=2))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
