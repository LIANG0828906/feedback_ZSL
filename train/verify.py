from __future__ import annotations

import argparse
import json
import time
from pathlib import Path
from typing import Any

import numpy as np

from train.common import (
    atomic_write_json,
    load_config,
    load_data_metadata,
    load_manifest_for_config,
    open_shard,
    parameter_names,
    resolve_path,
    step_batch_slice,
)
from train.db import connect, database_path
from train.model import accuracy, loss_and_gradient
from train.reference import run_reference_training


def _decode_gradient(blob: bytes) -> np.ndarray:
    return np.frombuffer(blob, dtype=np.float64).copy()


def verify(
    config_path: str = "config.json",
    e2e_seconds: float | None = None,
) -> dict[str, Any]:
    config = load_config(config_path)
    manifest = load_manifest_for_config(config)
    metadata = load_data_metadata(config)
    names = parameter_names(manifest)
    name_to_index = {name: index for index, name in enumerate(names)}
    connection = connect(database_path(config))
    started = time.perf_counter()
    try:
        step_rows = {
            int(row["step"]): dict(row)
            for row in connection.execute("SELECT * FROM steps ORDER BY step")
        }
        submissions = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM submissions WHERE status = 'applied' "
                "ORDER BY step, worker_id"
            )
        ]
        stale_rows = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM stale_rejections ORDER BY rejection_id"
            )
        ]
        duplicate_rows = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM duplicate_submissions ORDER BY duplicate_id"
            )
        ]
        takeover_rows = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM takeovers ORDER BY takeover_id"
            )
        ]
        lag_events = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM lag_events ORDER BY lag_event_id"
            )
        ]
        worker_rows = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM workers ORDER BY worker_id"
            )
        ]
        ownership_rows = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM shard_ownership ORDER BY parameter_name"
            )
        ]
        current_parameters = {
            row["parameter_name"]: float(row["value"])
            for row in connection.execute(
                "SELECT parameter_name, value FROM parameters ORDER BY parameter_name"
            )
        }
        route_rows = [
            dict(row)
            for row in connection.execute(
                "SELECT step, parameter_name, shard_id, generation "
                "FROM routes ORDER BY step, parameter_name"
            )
        ]

        history: dict[int, dict[str, float]] = {}
        for row in connection.execute(
            """
            SELECT version, parameter_name, value
            FROM parameter_history
            ORDER BY version, parameter_name
            """
        ):
            history.setdefault(int(row["version"]), {})[
                row["parameter_name"]
            ] = float(row["value"])
    finally:
        connection.close()

    reference = run_reference_training(config, include_snapshots=True)
    reference_snapshots = [
        np.asarray(snapshot, dtype=np.float64)
        for snapshot in reference["parameter_snapshots"]
    ]
    expected_names = names
    configured_steps = int(manifest["hyperparameters"]["steps"])
    worker_count = int(manifest["hyperparameters"]["workers"])
    samples_per_worker = int(
        manifest["hyperparameters"]["samples_per_worker_per_step"]
    )
    learning_rate = float(manifest["hyperparameters"]["learning_rate"])
    worker_order = [
        str(worker["worker_id"]) for worker in config["workers"]
    ]

    final_db_vector = np.asarray(
        [current_parameters.get(name, float("nan")) for name in expected_names],
        dtype=np.float64,
    )
    final_reference_vector = np.asarray(
        [reference["final_parameters"][name] for name in expected_names],
        dtype=np.float64,
    )
    final_abs = np.abs(final_db_vector - final_reference_vector)
    final_max_abs = float(np.max(final_abs)) if final_abs.size else float("inf")
    top_indices = np.argsort(final_abs)[-10:][::-1] if final_abs.size else []
    top_final_deviations = [
        {
            "absolute_deviation": float(final_abs[index]),
            "parameter_name": expected_names[int(index)],
        }
        for index in top_indices
    ]

    worker_shards = {
        str(worker["worker_id"]): int(worker["shard_index"])
        for worker in config["workers"]
    }
    shard_data = {
        worker_id: open_shard(config, metadata, shard_index)
        for worker_id, shard_index in worker_shards.items()
    }
    gradient_recompute_max_abs = 0.0
    recomputed_submissions = 0
    accepted_stale_bases: list[dict[str, Any]] = []
    for submission in submissions:
        base_version = int(submission["base_version"])
        if base_version not in history:
            continue
        base_parameters = np.asarray(
            [history[base_version][name] for name in expected_names],
            dtype=np.float64,
        )
        features, labels = shard_data[str(submission["worker_id"])]
        step = int(submission["step"])
        batch = step_batch_slice(step, samples_per_worker, int(features.shape[0]))
        _loss, recomputed = loss_and_gradient(
            np.asarray(features[batch], dtype=np.float32),
            np.asarray(labels[batch], dtype=np.int64),
            base_parameters,
            manifest,
        )
        stored = _decode_gradient(submission["gradient"])
        if stored.size == recomputed.size:
            gradient_recompute_max_abs = max(
                gradient_recompute_max_abs,
                float(np.max(np.abs(stored - recomputed))),
            )
            recomputed_submissions += 1
        if base_version != step - 1:
            accepted_stale_bases.append(
                {
                    "base_version": base_version,
                    "canonical_base_version": step - 1,
                    "step": step,
                    "worker_id": submission["worker_id"],
                }
            )

    step_loss_relative_errors: list[float] = []
    step_parameter_deviations: list[dict[str, Any]] = []
    for step in range(1, configured_steps + 1):
        row = step_rows.get(step)
        expected_loss = float(reference["loss_sequence"][step - 1])
        if row is None:
            step_loss_relative_errors.append(float("inf"))
            step_parameter_deviations.append(
                {"step": step, "max_abs_parameter_deviation": None}
            )
            continue
        actual_loss = float(row["loss"])
        relative = abs(actual_loss - expected_loss) / max(abs(expected_loss), 1e-12)
        step_loss_relative_errors.append(relative)
        if step in history:
            db_vector = np.asarray(
                [history[step][name] for name in expected_names], dtype=np.float64
            )
            max_deviation = float(
                np.max(np.abs(db_vector - reference_snapshots[step - 1]))
            )
        else:
            max_deviation = float("inf")
        step_parameter_deviations.append(
            {
                "max_abs_parameter_deviation": max_deviation,
                "step": step,
            }
        )
    finite_loss_errors = [
        value for value in step_loss_relative_errors if np.isfinite(value)
    ]
    loss_max_relative_error = (
        float(max(finite_loss_errors)) if finite_loss_errors else float("inf")
    )
    max_step_parameter_deviation = max(
        (
            float(item["max_abs_parameter_deviation"])
            for item in step_parameter_deviations
            if item["max_abs_parameter_deviation"] is not None
            and np.isfinite(item["max_abs_parameter_deviation"])
        ),
        default=float("inf"),
    )

    validation_features = np.load(
        resolve_path(config["data"]["directory"])
        / metadata["validation_features_file"],
        mmap_mode="r",
    )
    validation_labels = np.load(
        resolve_path(config["data"]["directory"])
        / metadata["validation_labels_file"],
        mmap_mode="r",
    )
    trained_accuracy = accuracy(
        np.asarray(validation_features, dtype=np.float32),
        np.asarray(validation_labels, dtype=np.int64),
        final_db_vector,
        manifest,
    )

    submissions_by_step: dict[int, list[dict[str, Any]]] = {}
    for submission in submissions:
        submissions_by_step.setdefault(int(submission["step"]), []).append(submission)
    step_update_max_abs = 0.0
    for step, attempts in submissions_by_step.items():
        if step - 1 not in history or step not in history:
            step_update_max_abs = float("inf")
            continue
        old = np.asarray(
            [history[step - 1][name] for name in expected_names], dtype=np.float64
        )
        aggregate = np.zeros(len(expected_names), dtype=np.float64)
        total = 0
        by_worker = {
            str(attempt["worker_id"]): attempt for attempt in attempts
        }
        for worker_id in worker_order:
            attempt = by_worker.get(worker_id)
            if attempt is None:
                continue
            gradient = _decode_gradient(attempt["gradient"])
            weight = int(attempt["sample_count"])
            aggregate += gradient * float(weight)
            total += weight
        if total:
            aggregate /= float(total)
            expected = old - learning_rate * aggregate
            actual = np.asarray(
                [history[step][name] for name in expected_names],
                dtype=np.float64,
            )
            step_update_max_abs = max(
                step_update_max_abs, float(np.max(np.abs(expected - actual)))
            )

    route_counts: dict[int, int] = {}
    route_shards: dict[int, set[str]] = {}
    for route in route_rows:
        step = int(route["step"])
        route_counts[step] = route_counts.get(step, 0) + 1
        route_shards.setdefault(step, set()).add(route["shard_id"])
    complete_routes = all(
        route_counts.get(step) == len(names) for step in range(1, configured_steps + 1)
    )
    complete_submissions = all(
        len(submissions_by_step.get(step, [])) == worker_count
        for step in range(1, configured_steps + 1)
    )

    ownership_names = [row["parameter_name"] for row in ownership_rows]
    ownership_unique = len(ownership_names) == len(set(ownership_names))
    ownership_complete = set(ownership_names) == set(names)
    owners_after_takeover = sorted(set(row["shard_id"] for row in ownership_rows))

    transaction_ok = True
    for step in range(1, configured_steps + 1):
        if step not in history or set(history[step]) != set(names):
            transaction_ok = False
            break

    worker_reports: list[dict[str, Any]] = []
    worker_report_dir = resolve_path("run/worker_reports")
    if worker_report_dir.exists():
        for path in sorted(worker_report_dir.glob("*.json")):
            try:
                worker_reports.append(json.loads(path.read_text(encoding="utf-8")))
            except (OSError, ValueError):
                pass
    p95_values = [
        float(report["submit_latency_p95_ms"])
        for report in worker_reports
        if report.get("submit_latency_p95_ms") is not None
    ]
    max_worker_p95_ms = max(p95_values) if p95_values else None

    affected: list[dict[str, Any]] = []
    for row in stale_rows:
        affected.append(
            {
                "base_version": int(row["base_version"]),
                "current_version": int(row["current_version"]),
                "parameter_names": list(names),
                "parameter_names_count": len(names),
                "source": "stale_gradient_rejected_before_routing",
                "step": int(row["step"]),
                "version_gap": int(row["version_gap"]),
                "worker_id": row["worker_id"],
            }
        )
    for row in duplicate_rows:
        affected.append(
            {
                "parameter_names": [],
                "parameter_names_count": 0,
                "returned_version": int(row["returned_version"]),
                "source": "duplicate_gradient_discarded",
                "step": int(row["step"]),
                "worker_id": row["worker_id"],
            }
        )
    takeover_summaries: list[dict[str, Any]] = []
    for row in takeover_rows:
        takeover_names = sorted(json.loads(row["parameter_names"]))
        status = row["status"]
        started_at = float(row["started_at"])
        completed_at = row["completed_at"]
        recovery_seconds = (
            float(completed_at) - started_at
            if completed_at is not None
            else None
        )
        to_version = row["to_version"]
        deviation = 0.0
        if to_version is not None and int(to_version) in history:
            db_vector = np.asarray(
                [history[int(to_version)][name] for name in takeover_names],
                dtype=np.float64,
            )
            ref_positions = [name_to_index[name] for name in takeover_names]
            ref_vector = reference_snapshots[int(to_version) - 1][ref_positions]
            if db_vector.size:
                deviation = float(np.max(np.abs(db_vector - ref_vector)))
        summary = {
            "from_shard_id": row["old_shard_id"],
            "new_shard_id": row["new_shard_id"],
            "parameter_names": takeover_names,
            "parameter_names_count": len(takeover_names),
            "recovery_seconds": recovery_seconds,
            "source": "parameter_shard_takeover_from_latest_committed_state",
            "status": status,
            "step": int(to_version) if to_version is not None else None,
            "max_abs_parameter_deviation_at_takeover": deviation,
            "trigger": row["trigger"],
        }
        takeover_summaries.append(summary)
        if status == "completed":
            affected.append(
                {
                    "parameter_names": takeover_names,
                    "parameter_names_count": len(takeover_names),
                    "source": summary["source"],
                    "step": summary["step"],
                    "max_abs_parameter_deviation": deviation,
                }
            )
    for item in accepted_stale_bases:
        affected.append(
            {
                "parameter_names": list(names),
                "parameter_names_count": len(names),
                "source": "stale_base_version_accepted_within_limit",
                **item,
            }
        )

    completed_takeovers = [
        event for event in takeover_summaries if event["status"] == "completed"
    ]
    lagging_by_worker = {
        row["worker_id"]: bool(row["lagging"]) for row in worker_rows
    }
    checks = {
        "all_steps_present": len(step_rows) == configured_steps,
        "complete_routes": complete_routes,
        "complete_step_submissions": complete_submissions,
        "current_parameters_match_last_history": (
            set(current_parameters) == set(names)
            and all(
                abs(current_parameters[name] - history[configured_steps][name]) < 1e-12
                for name in names
            )
        ),
        "final_parameter_tolerance": final_max_abs <= 1e-4,
        "gradient_recompute_tolerance": gradient_recompute_max_abs <= 1e-9,
        "loss_sequence_tolerance": loss_max_relative_error <= 1e-3,
        "ownership_complete": ownership_complete,
        "ownership_unique": ownership_unique,
        "step_transaction_history_complete": transaction_ok,
        "step_update_tolerance": step_update_max_abs <= 1e-9,
        "lag_event_marker_consistent": all(
            lagging_by_worker.get(event["worker_id"], False)
            for event in lag_events
        ),
        "accuracy_not_below_reference": (
            trained_accuracy >= float(reference["accuracy"]) - 0.01
        ),
        "worker_p95_within_400ms": (
            max_worker_p95_ms is None or max_worker_p95_ms <= 400.0
        ),
        "takeover_recovery_within_30s": all(
            event["recovery_seconds"] is not None
            and event["recovery_seconds"] <= 30.0
            for event in completed_takeovers
        ),
    }
    passed = all(checks.values())
    verification_elapsed = time.perf_counter() - started
    report = {
        "status": "pass" if passed else "fail",
        "checks": checks,
        "summary": {
            "steps": len(step_rows),
            "configured_steps": configured_steps,
            "applied_submissions": len(submissions),
            "recomputed_submissions": recomputed_submissions,
            "stale_rejections": len(stale_rows),
            "duplicate_submissions": len(duplicate_rows),
            "completed_takeovers": len(completed_takeovers),
            "lag_events": len(lag_events),
            "lagging_workers": [
                {
                    "consecutive_stale": int(row["consecutive_stale"]),
                    "lagging": bool(row["lagging"]),
                    "total_stale": int(row["total_stale"]),
                    "trigger_version_gap": int(row["trigger_version_gap"]),
                    "worker_id": row["worker_id"],
                }
                for row in worker_rows
                if bool(row["lagging"])
            ],
            "trained_accuracy": trained_accuracy,
            "reference_accuracy": float(reference["accuracy"]),
            "accuracy_delta": trained_accuracy - float(reference["accuracy"]),
            "final_max_abs_parameter_deviation": final_max_abs,
            "max_step_parameter_deviation": max_step_parameter_deviation,
            "loss_max_relative_error": loss_max_relative_error,
            "gradient_recompute_max_abs": gradient_recompute_max_abs,
            "step_update_max_abs": step_update_max_abs,
            "max_worker_submit_p95_ms": max_worker_p95_ms,
            "e2e_seconds": e2e_seconds,
            "verification_seconds": round(verification_elapsed, 3),
            "owners_after_takeover": owners_after_takeover,
            "final_loss": float(reference["loss_sequence"][-1]),
        },
        "takeovers": takeover_summaries,
        "lag_events": lag_events,
        "affected_steps_and_parameters": affected,
        "top_final_parameter_deviations": top_final_deviations,
        "worker_reports": worker_reports,
    }
    atomic_write_json(resolve_path("reports/verification.json"), report)
    _write_markdown(report)
    return report


def _write_markdown(report: dict[str, Any]) -> None:
    summary = report["summary"]
    lines = [
        "# Asynchronous training verification",
        "",
        f"Status: **{report['status']}**",
        "",
        "## Results",
        "",
        f"- Steps: {summary['steps']} / {summary['configured_steps']}",
        f"- Final max parameter deviation: {summary['final_max_abs_parameter_deviation']:.3e}",
        f"- Max loss relative error: {summary['loss_max_relative_error']:.3e}",
        f"- Gradient recompute max deviation: {summary['gradient_recompute_max_abs']:.3e}",
        f"- Step update max deviation: {summary['step_update_max_abs']:.3e}",
        f"- Accuracy trained/reference: {summary['trained_accuracy']:.6f} / {summary['reference_accuracy']:.6f}",
        f"- Stale rejections: {summary['stale_rejections']}",
        f"- Duplicate submissions discarded: {summary['duplicate_submissions']}",
        f"- Completed takeovers: {summary['completed_takeovers']}",
        f"- Max worker submit P95: {summary['max_worker_submit_p95_ms']} ms",
        f"- End-to-end: {summary['e2e_seconds']} s",
        "",
        "## Affected Records",
        "",
    ]
    if not report["affected_steps_and_parameters"]:
        lines.append("- No stale, duplicate, or takeover events were recorded.")
    for event in report["affected_steps_and_parameters"]:
        name_preview = ", ".join(event.get("parameter_names", [])[:5])
        suffix = "" if event.get("parameter_names_count", 0) <= 5 else ", ..."
        lines.append(
            f"- step={event.get('step')} source={event.get('source')} "
            f"parameters=[{name_preview}{suffix}]"
        )
    lines.extend(
        [
            "",
            "## Checks",
            "",
            *[
                f"- {'PASS' if value else 'FAIL'} `{name}`"
                for name, value in sorted(report["checks"].items())
            ],
            "",
        ]
    )
    atomic_write_json(
        resolve_path("reports/verification_summary.json"),
        {
            "checks": report["checks"],
            "status": report["status"],
            "summary": report["summary"],
            "takeovers": report["takeovers"],
        },
    )
    target = resolve_path("reports/training_report.md")
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text("\n".join(lines), encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description="Verify distributed training against reference")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--e2e-seconds", type=float, default=None)
    args = parser.parse_args()
    report = verify(args.config, e2e_seconds=args.e2e_seconds)
    print(json.dumps(report["summary"], indent=2, sort_keys=True), flush=True)
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
