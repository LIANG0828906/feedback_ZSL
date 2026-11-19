"""Prometheus text exposition and the reproducible sync report."""

from __future__ import annotations

from .link import percentile
from .store import SyncStore


def _escape(value: str) -> str:
    return value.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')


def _labels(**pairs) -> str:
    ordered = ",".join(f'{key}="{_escape(str(value))}"' for key, value in sorted(pairs.items()))
    return "{" + ordered + "}"


def render_prometheus(
    config,
    store: SyncStore,
    link_states: dict,
    health_snapshot: dict,
    task_counts: dict,
    switch_counts: dict,
) -> str:
    lines: list[str] = []

    def emit(name: str, help_text: str, metric_type: str, samples: list[tuple[dict, float]]) -> None:
        lines.append(f"# HELP {name} {help_text}")
        lines.append(f"# TYPE {name} {metric_type}")
        for labels, value in samples:
            lines.append(f"{name}{_labels(**labels)} {value}")

    byte_samples = []
    shard_samples = []
    retrans_samples = []
    rtt_samples = []
    switch_samples = []
    conn_samples = []
    for egress in config.egresses:
        egress_id = egress.egress_id
        state = link_states[egress_id]
        monitor = health_snapshot[egress_id]
        with state.lock:
            bytes_sent = int(state.emitted_bytes)
            total_segments = int(state.total_segments)
            lost_segments = int(state.lost_segments)
            active = int(state.active_connections)
        # Shard bytes/shards from authoritative shard table.
        shard_rows = store.fetch_all(
            "SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(length), 0) FROM shards "
            "WHERE final_egress=? AND status='succeeded'",
            (egress_id,),
        )
        shard_count, shard_bytes = int(shard_rows[0][0]), int(shard_rows[0][1] or 0)
        byte_samples.append(({"egress": egress_id}, shard_bytes or bytes_sent))
        shard_samples.append(({"egress": egress_id}, shard_count))
        rate = round(lost_segments / total_segments, 6) if total_segments else 0.0
        retrans_samples.append(({"egress": egress_id}, rate))
        rtt_samples.append(({"egress": egress_id}, monitor.get("rtt_median_ms") or 0.0))
        switch_samples.append(({"egress": egress_id}, switch_counts.get(egress_id, 0)))
        conn_samples.append(({"egress": egress_id}, active))
    emit("edge_sync_egress_uploaded_bytes_total", "Bytes uploaded per egress.", "counter", byte_samples)
    emit("edge_sync_egress_shards_total", "Successful shards per egress.", "counter", shard_samples)
    emit("edge_sync_egress_retransmit_rate", "Link-level retransmit ratio per egress.", "gauge", retrans_samples)
    emit("edge_sync_egress_rtt_median_ms", "RTT median in the health window per egress.", "gauge", rtt_samples)
    emit("edge_sync_egress_switch_total", "Switch events per egress.", "counter", switch_samples)
    emit("edge_sync_egress_active_connections", "Active proxied connections per egress.", "gauge", conn_samples)

    task_samples = []
    for task_id in sorted(task_counts):
        counts = task_counts[task_id]
        task_samples.append(({"task": task_id}, counts.get("succeeded", 0)))
    emit("edge_sync_task_shards_completed_total", "Completed shards per task.", "counter", task_samples)

    task_progress: list[tuple[dict, float]] = []
    for task_id in sorted(task_counts):
        counts = task_counts[task_id]
        done = counts.get("succeeded", 0)
        total = sum(counts.values())
        task_progress.append(({"task": task_id}, round(done / total, 6) if total else 0.0))
    emit("edge_sync_task_progress_ratio", "Shard completion ratio per task.", "gauge", task_progress)
    return "\n".join(lines) + "\n"


def _stable_attempts(store: SyncStore) -> list[dict]:
    rows = store.fetch_all(
        "SELECT task_id, shard_seq, attempt_no, egress, started_ms, ended_ms, success, "
        "duplicate, error_code, signal_json, link_json FROM attempts ORDER BY task_id, shard_seq, attempt_no"
    )
    import json

    attempts = []
    for row in rows:
        attempts.append(
            {
                "task_id": row[0],
                "shard_seq": row[1],
                "attempt_no": row[2],
                "egress": row[3],
                "started_ms": row[4],
                "ended_ms": row[5],
                "success": bool(row[6]),
                "duplicate": bool(row[7]),
                "error_code": row[8],
                "signal": json.loads(row[9]) if row[9] else None,
                "link": json.loads(row[10]) if row[10] else None,
            }
        )
    return attempts


def build_report(config, store: SyncStore, link_states: dict, health_snapshot: dict) -> dict:
    attempts = _stable_attempts(store)
    egress_ids = sorted(egress.egress_id for egress in config.egresses)
    per_egress = {
        egress_id: {
            "bytes": 0,
            "shards": 0,
            "attempts": 0,
            "retrans_segments": 0,
            "rto_count": 0,
            "rtt_samples_ms": [],
        }
        for egress_id in egress_ids
    }
    for attempt in attempts:
        bucket = per_egress[attempt["egress"]]
        bucket["attempts"] += 1
        if attempt["success"]:
            bucket["shards"] += 1
            shard_rows = store.fetch_all(
                "SELECT length FROM shards WHERE task_id=? AND shard_seq=?",
                (attempt["task_id"], attempt["shard_seq"]),
            )
            bucket["bytes"] += int(shard_rows[0][0])
        if attempt["link"]:
            bucket["retrans_segments"] += int(attempt["link"].get("retrans_segments", 0))
            bucket["rto_count"] += int(attempt["link"].get("rto_count", 0))
            bucket["rtt_samples_ms"].extend(attempt["link"].get("rtt_samples_ms", []))

    egress_report = {}
    for egress_id in egress_ids:
        bucket = per_egress[egress_id]
        samples = sorted(bucket.pop("rtt_samples_ms"))
        egress_report[egress_id] = {
            **bucket,
            "healthy": health_snapshot[egress_id]["healthy"],
            "rtt_p50_ms": percentile(samples, 50),
            "rtt_p90_ms": percentile(samples, 90),
            "rtt_p99_ms": percentile(samples, 99),
        }

    switch_rows = store.fetch_all(
        "SELECT event_seq, egress, target_egress, reason_metric, observed_value, threshold_value, "
        "shard_count, bad_samples, emulated_bad_ms FROM switches ORDER BY event_seq"
    )
    switches = [
        {
            "event_seq": row[0],
            "egress": row[1],
            "target_egress": row[2],
            "reason_metric": row[3],
            "observed_value": row[4],
            "threshold_value": row[5],
            "shard_count": row[6],
            "bad_samples": row[7],
            "emulated_bad_ms": row[8],
        }
        for row in switch_rows
    ]

    task_rows = store.fetch_all(
        "SELECT t.task_id, t.total_shards, t.total_bytes, t.status, "
        "COALESCE(SUM(CASE WHEN s.status='succeeded' THEN 1 ELSE 0 END), 0), "
        "COALESCE(SUM(CASE WHEN s.status='failed' THEN 1 ELSE 0 END), 0) "
        "FROM tasks t LEFT JOIN shards s USING (task_id) GROUP BY t.task_id ORDER BY t.task_id"
    )
    tasks = []
    for row in task_rows:
        tasks.append(
            {
                "task_id": row[0],
                "total_shards": row[1],
                "total_bytes": row[2],
                "status": row[3],
                "succeeded_shards": row[4],
                "failed_shards": row[5],
            }
        )

    totals = {
        "tasks": len(tasks),
        "succeeded_shards": sum(task["succeeded_shards"] for task in tasks),
        "failed_shards": sum(task["failed_shards"] for task in tasks),
        "retrans_segments": sum(item["retrans_segments"] for item in egress_report.values()),
        "rto_count": sum(item["rto_count"] for item in egress_report.values()),
        "uploaded_bytes": sum(item["bytes"] for item in egress_report.values()),
        "switch_events": len(switches),
    }
    return {
        "schema_version": 1,
        "seed": config.seed,
        "totals": totals,
        "tasks": tasks,
        "egresses": egress_report,
        "switches": switches,
        "deterministic_fields": [
            "totals", "tasks", "egresses", "switches", "seed", "schema_version",
        ],
    }
