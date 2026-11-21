"""Bake orchestration: incremental rebake + scheduling + atomic publish."""
from __future__ import annotations

import json
import os
import signal
import threading
from pathlib import Path

from . import batch as batchmod
from .budget import estimate_task_bytes, fit_within_budget
from .config import BakeConfig
from .incremental import compute_affected_set, compute_param_version
from .pipeline import bake_material, build_chain_context, material_param_hash, max_code_delta
from .scheduler import Scheduler


class TraceLog:
    """JSONL trace: material index, param version, affected/reused counts,
    quota usage, degradation stages, content hashes, scheduler decisions."""

    def __init__(self, path):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()

    def __call__(self, record: dict):
        with self._lock:
            with open(self.path, "a", encoding="utf-8") as fh:
                fh.write(json.dumps(record, sort_keys=True) + "\n")


def run_bake(
    config: BakeConfig,
    materials: list,
    batch_id: str,
    simulate_kill_after: int | None = None,
) -> dict:
    """Run one batch and atomically publish it. Returns the manifest."""
    trace = TraceLog(config.trace_path)
    ctx = build_chain_context(config)
    new_version = compute_param_version(config, materials)
    version_token = new_version["global"][:16]

    previous = batchmod.load_latest_valid(config.snapshots_dir)
    old_version = previous["param_version"] if previous else None
    old_entries = {e["material_id"]: e for e in previous["entries"]} if previous else {}

    affected_ids, reasons = compute_affected_set(old_version, new_version)
    affected_set = set(affected_ids)
    affected = [m for m in materials if m.material_id in affected_set]
    reused = [m for m in materials if m.material_id in old_entries and m.material_id not in affected_set]

    # Reuse spheres committed by a killed earlier attempt of this batch.
    journal_records = batchmod.load_journal(config.snapshots_dir, batch_id)
    journal_done: dict[str, dict] = {}
    current_param = {m.material_id: material_param_hash(m) for m in materials}
    for record in journal_records:
        entry = record["entry"]
        if (
            record["param_version"] == version_token
            and entry.get("material_param_hash") == current_param.get(entry["material_id"])
        ):
            journal_done[record["entry"]["material_id"]] = record["entry"]

    trace(
        {
            "event": "batch_start",
            "batch_id": batch_id,
            "param_version": version_token,
            "affected_count": len(affected),
            "reused_count": len(reused),
            "journal_reused_count": len(journal_done),
            "reasons": reasons,
        }
    )

    # Budget: one shared degradation decision per batch (same shape for all).
    shape, degradation_log = fit_within_budget(config)
    task_bytes = estimate_task_bytes(shape)
    for entry in degradation_log:
        trace({"event": "degrade", "batch_id": batch_id, **entry})

    todo = [m for m in affected if m.material_id not in journal_done]
    tasks = [
        {
            "submission_index": m.material_index,
            "material": m,
            "memory_bytes": task_bytes,
        }
        for m in todo
    ]

    scheduler = Scheduler(
        parallelism=config.parallelism,
        global_memory_bytes=config.global_memory_bytes,
        trace=trace,
        verify_order=config.verify_order,
    )
    killed = {"count": 0}

    def worker(task):
        material = task["material"]
        result = bake_material(material, ctx, config)
        batchmod.append_journal(config.snapshots_dir, batch_id, result, version_token)
        trace(
            {
                "event": "sphere_committed",
                "batch_id": batch_id,
                "material_id": material.material_id,
                "material_index": material.material_index,
                "param_version": version_token,
                "content_hash": result["content_hash"],
                "usage_bytes": task["memory_bytes"],
                "degradation_stages": [d["stage"] for d in degradation_log],
            }
        )
        if simulate_kill_after is not None:
            killed["count"] += 1
            if killed["count"] >= simulate_kill_after:
                os.kill(os.getpid(), signal.SIGKILL)
        return result

    results = scheduler.run_batch(batch_id, tasks, worker) if tasks else []

    # Assemble the full entry list in fixed material-index order.
    baked = {r["material_id"]: r for r in results}
    baked.update(journal_done)
    index_of = {m.material_id: m.material_index for m in materials}
    entries = []
    for m in materials:
        if m.material_id in baked:
            entry = dict(baked[m.material_id])
        elif m.material_id in old_entries:
            entry = dict(old_entries[m.material_id])
        else:
            continue
        entry["material_index"] = index_of[m.material_id]
        entries.append(entry)
    entries.sort(key=lambda e: e["material_index"])

    delta = max_code_delta(previous["entries"], entries) if previous else 0
    manifest = {
        "batch_id": batch_id,
        "param_version": new_version,
        "entries": entries,
        "hash_chain": batchmod.build_hash_chain(batch_id, entries),
        "quota": {
            "per_task_bytes": task_bytes,
            "per_task_quota_bytes": config.per_task_memory_bytes,
            "global_quota_bytes": config.global_memory_bytes,
            "parallelism": config.parallelism,
        },
        "degradation": degradation_log,
        "chain_step_hashes": ctx.step_hashes,
        "max_code_delta_vs_previous": delta,
        "affected_count": len(affected),
        "reused_count": len(reused),
    }
    path = batchmod.commit_batch(config.snapshots_dir, manifest)
    batchmod.discard_journal(config.snapshots_dir, batch_id)
    trace(
        {
            "event": "batch_published",
            "batch_id": batch_id,
            "param_version": version_token,
            "affected_count": len(affected),
            "reused_count": len(reused),
            "max_code_delta_vs_previous": delta,
            "manifest": str(path),
            "chain_tip": manifest["hash_chain"][-1],
        }
    )
    return manifest
