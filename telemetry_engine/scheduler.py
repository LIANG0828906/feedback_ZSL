"""Quota-arbitrating multi-tenant task scheduler with fault recovery.

A shared pool of worker processes executes logical processing-unit tasks
for all tenants. Per-tenant quotas (memory, concurrent units) are enforced
at dispatch time. Crashed workers are detected, their in-flight tasks are
requeued (already committed tasks are never recomputed), and tenants whose
tasks exhaust retries fail with a stable business error code.
"""

import multiprocessing as mp
import queue as queue_mod
from collections import deque

from . import errors
from .worker import worker_main


class TenantExecution:
    def __init__(self, tenant_cfg, tasks, store, engine_cfg):
        self.tenant_id = tenant_cfg["tenant_id"]
        self.mem_quota = tenant_cfg["memory_quota_bytes"]
        self.max_units = tenant_cfg["max_units"]
        self.store = store
        self.max_retries = engine_cfg["max_retries"]
        self.row_bytes = engine_cfg["row_bytes"]
        self.mem_factor = engine_cfg["mem_factor"]
        committed = store.committed_task_ids()
        self.pre_committed = len(committed)
        self.pending = deque(t for t in tasks if t["task_id"] not in committed)
        self.planned = len(tasks)
        self.task_by_id = {t["task_id"]: t for t in tasks}
        self.inflight = {}
        self.inflight_bytes = 0
        self.executed = 0
        self.retries = 0
        self.crashes = 0
        self.failed = None
        self.done = False
        self.unit_stats = {}
        for task in tasks:
            stat = self.unit_stats.setdefault(
                task["unit"], {"tasks": 0, "rows": 0, "peak_mem_bytes": 0})
            stat["tasks"] += 1
            stat["rows"] += task["rows"]

    def estimate(self, task):
        return task["rows"] * self.row_bytes * self.mem_factor

    def fail(self, code):
        if self.failed is None:
            self.failed = code
            self.pending.clear()


def _fault_maps(fault_cfg, tenant_id):
    crash = {}
    for spec in fault_cfg.get("crash", []):
        if spec.get("tenant") == tenant_id:
            crash[spec["task_index"]] = spec.get("times", 1)
    always = set()
    for spec in fault_cfg.get("always_fail", []):
        if spec.get("tenant") == tenant_id:
            always.add(spec["task_index"])
    return crash, always


def execute(tenants, engine_cfg, fault_cfg, log=print):
    """Run all tenant tasks concurrently on a shared worker pool."""
    ctx = mp.get_context("spawn")
    n_workers = engine_cfg["worker_processes"]
    window_ms = engine_cfg["window_seconds"] * 1000
    result_q = ctx.Queue()
    workers = {}

    def spawn(wid):
        tq = ctx.Queue()
        proc = ctx.Process(
            target=worker_main,
            args=(tq, result_q, window_ms, engine_cfg["row_bytes"],
                  engine_cfg["mem_factor"]),
            daemon=True,
        )
        proc.start()
        workers[wid] = {"proc": proc, "queue": tq, "stopped": False}

    for wid in range(n_workers):
        spawn(wid)

    faults = {t.tenant_id: _fault_maps(fault_cfg, t.tenant_id) for t in tenants}
    by_id = {t.tenant_id: t for t in tenants}
    capacity = n_workers * 2
    rr = 0

    def inflight_total():
        return sum(len(t.inflight) for t in tenants)

    try:
        while True:
            for tenant in tenants:
                if tenant.failed or tenant.done:
                    continue
                while (tenant.pending
                       and len(tenant.inflight) < tenant.max_units
                       and inflight_total() < capacity):
                    task = tenant.pending[0]
                    est = tenant.estimate(task)
                    if est > tenant.mem_quota:
                        tenant.fail(errors.E_MEMORY_QUOTA_EXCEEDED)
                        log(f"[{tenant.tenant_id}] task {task['task_id']} needs "
                            f"{est} bytes > quota {tenant.mem_quota}: "
                            f"{errors.E_MEMORY_QUOTA_EXCEEDED}")
                        break
                    if tenant.inflight_bytes + est > tenant.mem_quota:
                        break
                    tenant.pending.popleft()
                    task["attempts"] += 1
                    msg = {k: v for k, v in task.items()}
                    crash_map, always = faults[tenant.tenant_id]
                    idx = task["index"]
                    if idx in always or task["attempts"] <= crash_map.get(idx, 0):
                        msg["inject_crash"] = True
                    wid = rr % n_workers
                    rr += 1
                    tenant.inflight[task["task_id"]] = {"worker": wid, "est": est}
                    tenant.inflight_bytes += est
                    workers[wid]["queue"].put(msg)

            for tenant in tenants:
                if not tenant.failed and not tenant.pending and not tenant.inflight:
                    tenant.done = True
            if (all(t.done or t.failed for t in tenants)
                    and inflight_total() == 0):
                break

            try:
                res = result_q.get(timeout=0.05)
            except queue_mod.Empty:
                res = None
            if res is not None:
                tenant = by_id[res["tenant_id"]]
                info = tenant.inflight.pop(res["task_id"], None)
                if info is None:
                    res = None  # duplicate/late result of a requeued task
                else:
                    tenant.inflight_bytes -= info["est"]
            if res is not None:
                if "error" in res:
                    task = tenant.task_by_id[res["task_id"]]
                    _requeue(tenant, task, errors.E_INTERNAL, log)
                elif not tenant.failed:
                    task = tenant.task_by_id[res["task_id"]]
                    tenant.store.commit(task, res["partials"], res["attempts"])
                    tenant.executed += 1
                    stat = tenant.unit_stats[task["unit"]]
                    if res["peak_mem_bytes"] > stat["peak_mem_bytes"]:
                        stat["peak_mem_bytes"] = res["peak_mem_bytes"]

            for wid, worker in workers.items():
                if worker["stopped"] or worker["proc"].is_alive():
                    continue
                worker["stopped"] = True
                log(f"[scheduler] worker {wid} exited "
                    f"(code {worker['proc'].exitcode}); rescheduling tasks")
                for tenant in tenants:
                    lost = [tid for tid, info in tenant.inflight.items()
                            if info["worker"] == wid]
                    for tid in lost:
                        info = tenant.inflight.pop(tid)
                        tenant.inflight_bytes -= info["est"]
                        if tenant.done or tenant.failed:
                            continue
                        tenant.crashes += 1
                        _requeue(tenant, tenant.task_by_id[tid],
                                 errors.E_TASK_RETRY_EXHAUSTED, log)
                spawn(wid)
    finally:
        for worker in workers.values():
            try:
                worker["queue"].put(None)
            except Exception:
                pass
        for worker in workers.values():
            worker["proc"].join(timeout=5)
            if worker["proc"].is_alive():
                worker["proc"].terminate()


def _requeue(tenant, task, exhaust_code, log):
    if task["attempts"] > tenant.max_retries:
        tenant.fail(exhaust_code)
        log(f"[{tenant.tenant_id}] task {task['task_id']} exhausted "
            f"{tenant.max_retries} retries: {exhaust_code}")
        return
    task["inject_crash"] = False
    tenant.pending.appendleft(task)
    tenant.retries += 1
