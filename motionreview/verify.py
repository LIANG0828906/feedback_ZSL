"""End-to-end sample verification against generated baselines."""

from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path
from typing import Any

from .bvh import parse_bvh_file
from .config import RunConfig
from .generator import load_case_constraints
from .scheduler import ClipTask, Scheduler
from .storage import SnapshotStore


def _read_jsonl(path: Path) -> dict[int, dict[str, Any]]:
    records: dict[int, dict[str, Any]] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        item = json.loads(line)
        records[int(item["frame"])] = item
    return records


def _compare_committed(
    store: SnapshotStore,
    baseline: dict[int, dict[str, Any]],
) -> tuple[list[str], int, int | None]:
    errors: list[str] = []
    state = store.recover()
    for solution in state.solutions:
        expected = baseline.get(solution.frame)
        if expected is None:
            errors.append(f"frame {solution.frame}: missing baseline")
            continue
        if solution.pose_hash != expected["pose_hash"]:
            errors.append(f"frame {solution.frame}: pose hash differs")
        if solution.content_hash != expected["content_hash"]:
            errors.append(f"frame {solution.frame}: content hash differs")
        residual_ids = [
            (item["constraint_id"], item["residual"])
            for item in solution.residuals
        ]
        expected_ids = [
            (item["constraint_id"], item["residual"])
            for item in expected["residuals"]
        ]
        if residual_ids != expected_ids:
            errors.append(f"frame {solution.frame}: residuals differ")
    diagnostics = store.list_diagnostics()
    conflict_frame: int | None = None
    if diagnostics:
        diagnostic = json.loads(
            diagnostics[0].read_text(encoding="utf-8")
        )
        conflict_frame = int(diagnostic["frame"])
        expected = baseline.get(conflict_frame)
        if expected is None:
            errors.append(
                f"frame {conflict_frame}: conflict baseline missing"
            )
        else:
            if diagnostic.get("pose_hash") != expected["pose_hash"]:
                errors.append(
                    f"frame {conflict_frame}: conflict pose hash differs"
                )
            if diagnostic.get("content_hash") != expected["content_hash"]:
                errors.append(
                    f"frame {conflict_frame}: conflict content hash differs"
                )
            diagnostic_ids = [
                (item["constraint_id"], item["residual"])
                for item in diagnostic["residuals"]
            ]
            expected_ids = [
                (item["constraint_id"], item["residual"])
                for item in expected["residuals"]
            ]
            if diagnostic_ids != expected_ids:
                errors.append(
                    f"frame {conflict_frame}: conflict residuals differ"
                )
    return errors, len(state.solutions), conflict_frame


def verify_dataset(
    sample_root: str | Path,
    *,
    config: RunConfig,
    parallel_output: str | Path,
    serial_output: str | Path,
) -> dict[str, Any]:
    root = Path(sample_root)
    dataset = json.loads(
        (root / "manifest.json").read_text(encoding="utf-8")
    )
    tasks: list[ClipTask] = []
    baselines: dict[str, dict[int, dict[str, Any]]] = {}
    case_meta: dict[str, dict[str, Any]] = {}
    for case in dataset["cases"]:
        bvh_path = root / case["bvh"]
        constraints_path = root / case["constraints"]
        document = parse_bvh_file(
            bvh_path, clip_id=case["name"]
        )
        constraints = load_case_constraints(document, constraints_path)
        tasks.append(
            ClipTask(case["name"], document, constraints)
        )
        baseline_path = root / (case["baseline_file"] or "")
        baselines[case["name"]] = _read_jsonl(baseline_path)
        case_meta[case["name"]] = case

    parallel_config = config
    serial_config = replace(config, max_parallelism=1)
    with Scheduler(parallel_config, parallel_output) as scheduler:
        parallel_report = scheduler.run(tasks)
    with Scheduler(serial_config, serial_output) as scheduler:
        serial_report = scheduler.run(tasks)

    parallel_by_clip = {
        report.clip_id: report for report in parallel_report.reports
    }
    serial_by_clip = {
        report.clip_id: report for report in serial_report.reports
    }
    case_results: list[dict[str, Any]] = []
    overall_errors: list[str] = []
    for task in tasks:
        name = task.clip_id
        parallel_task = parallel_by_clip[name]
        serial_task = serial_by_clip[name]
        errors: list[str] = []
        for label, task_report in (
            ("parallel", parallel_task),
            ("serial", serial_task),
        ):
            if task_report.status not in {
                "COMPLETED",
                "CONFLICT_STOPPED",
            }:
                errors.append(
                    f"{label}: status {task_report.status}"
                )
        parallel_errors, parallel_frames, conflict_frame = _compare_committed(
            SnapshotStore(parallel_output, name), baselines[name]
        )
        serial_errors, serial_frames, _ = _compare_committed(
            SnapshotStore(serial_output, name), baselines[name]
        )
        errors.extend(parallel_errors)
        errors.extend(serial_errors)
        if parallel_frames != serial_frames:
            errors.append("parallel and serial committed frame count differs")
        parallel_hashes = {
            solution.frame: solution.content_hash
            for solution in SnapshotStore(
                parallel_output, name
            ).recover().solutions
        }
        serial_hashes = {
            solution.frame: solution.content_hash
            for solution in SnapshotStore(
                serial_output, name
            ).recover().solutions
        }
        if parallel_hashes != serial_hashes:
            errors.append("parallel and serial frame hashes differ")
        if case_meta[name].get("conflict"):
            expected_conflicts = case_meta[name].get(
                "conflict_frames", []
            )
            if expected_conflicts and conflict_frame is None:
                errors.append("expected conflict was not diagnosed")
            if conflict_frame is not None:
                if conflict_frame != expected_conflicts[0]:
                    errors.append(
                        "conflict frame differs from baseline"
                    )
                if parallel_task.next_frame != conflict_frame:
                    errors.append(
                        "frames after conflict were committed"
                    )
        else:
            if conflict_frame is not None:
                errors.append("unexpected conflict diagnostic")
            if parallel_frames != task.document.frame_count:
                errors.append("not all frames were committed")
        if errors:
            overall_errors.extend(f"{name}: {error}" for error in errors)
        case_results.append(
            {
                "name": name,
                "frames_verified": parallel_frames,
                "conflict_frame": conflict_frame,
                "parallel_status": parallel_task.status,
                "serial_status": serial_task.status,
                "degradation_stage": parallel_task.stage_name,
                "degraded": parallel_task.degraded,
                "errors": errors,
            }
        )
    return {
        "ok": not overall_errors,
        "config_fingerprint": config.fingerprint,
        "parallelism": config.max_parallelism,
        "cases": case_results,
        "errors": overall_errors,
    }
