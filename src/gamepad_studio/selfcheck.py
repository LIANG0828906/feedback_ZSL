"""End-to-end acceptance checks and reproducible validation reporting."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import traceback
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

import numpy
import PySide6

from .compare import compare_results
from .concurrency import ReplayCoordinator, SerializedSnapshotWriter
from .errors import (
    DivergenceError,
    MappingError,
    SampleValidationError,
    StudioError,
)
from .models import CurveSpec
from .pipeline import Pipeline
from .replay import check_truth, replay_full
from .samples import generate_samples
from .session import HotSwitchSession, plan_hot_switch
from .snapshot import SnapshotStore
from .validation import load_sample, write_sample_json


def _json_safe(value: Any) -> Any:
    if isinstance(value, Path):
        return str(value)
    if isinstance(value, tuple):
        return [_json_safe(item) for item in value]
    if isinstance(value, list):
        return [_json_safe(item) for item in value]
    if isinstance(value, dict):
        return {str(key): _json_safe(item) for key, item in value.items()}
    return value


def _run_check(
    checks: list[dict[str, Any]],
    name: str,
    function: Callable[[], dict[str, Any]],
) -> None:
    try:
        details = function()
        checks.append(
            {
                "name": name,
                "passed": bool(details.pop("passed", True)),
                "details": _json_safe(details),
            }
        )
    except Exception as exc:  # pragma: no cover - report path needs to be total
        checks.append(
            {
                "name": name,
                "passed": False,
                "details": {
                    "exception": type(exc).__name__,
                    "message": str(exc),
                    "traceback": traceback.format_exc(limit=8),
                },
            }
        )


def _copy_sample(source: Path) -> Path:
    temporary = Path(tempfile.mkdtemp(prefix="padweave-validation-"))
    target = temporary / source.name
    shutil.copytree(source, target)
    return target


def _expect_validation_code(
    sample_path: Path,
    mutate: Callable[[Path], None],
) -> dict[str, Any]:
    working = _copy_sample(sample_path)
    mutate(working)
    try:
        load_sample(working)
    except SampleValidationError as exc:
        return {
            "passed": True,
            "code": exc.code,
            "field": exc.field,
            "event_seq": exc.event_seq,
            "issues": [issue.to_dict() for issue in exc.issues],
        }
    finally:
        shutil.rmtree(working.parent, ignore_errors=True)
    return {
        "passed": False,
        "code": None,
        "field": None,
        "event_seq": None,
    }


def _mutate_missing_field(sample_path: Path) -> None:
    events_path = sample_path / "events.json"
    payload = json.loads(events_path.read_text(encoding="utf-8"))
    for event in payload["events"]:
        if event["type"] == "axis":
            event.pop("value", None)
            break
    events_path.write_text(json.dumps(payload, indent=2), encoding="utf-8")


def _mutate_unknown_axis(sample_path: Path) -> None:
    events_path = sample_path / "events.json"
    payload = json.loads(events_path.read_text(encoding="utf-8"))
    for event in payload["events"]:
        if event["type"] == "axis":
            event["axis"] = "not_a_declared_axis"
            break
    events_path.write_text(json.dumps(payload, indent=2), encoding="utf-8")


def _mutate_timestamp_order(sample_path: Path) -> None:
    events_path = sample_path / "events.json"
    payload = json.loads(events_path.read_text(encoding="utf-8"))
    if len(payload["events"]) > 1:
        payload["events"][1]["t_ms"] = payload["events"][0]["t_ms"] - 1
    events_path.write_text(json.dumps(payload, indent=2), encoding="utf-8")


def _mutate_bad_window(sample_path: Path) -> None:
    bindings_path = sample_path / "bindings.json"
    payload = json.loads(bindings_path.read_text(encoding="utf-8"))
    for rule in payload["rules"]:
        if rule["kind"] == "combo":
            rule["window_ms"] = 0
            break
    bindings_path.write_text(json.dumps(payload, indent=2), encoding="utf-8")


def _run_ui_smoke(project_root: Path, screenshot: Path) -> dict[str, Any]:
    env = os.environ.copy()
    source_path = str((project_root / "src").resolve())
    env["PYTHONPATH"] = source_path + (
        os.pathsep + env["PYTHONPATH"] if env.get("PYTHONPATH") else ""
    )
    command = [
        sys.executable,
        "-m",
        "gamepad_studio.app",
        "--smoke-test",
        "--sample",
        str(project_root / "samples" / "dual_stick"),
        "--screenshot",
        str(screenshot),
        "--samples",
        str(project_root / "samples"),
        "--snapshot-root",
        str(project_root / "artifacts" / "snapshots"),
    ]
    completed = subprocess.run(
        command,
        cwd=project_root,
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    payload: dict[str, Any] = {
        "passed": completed.returncode == 0 and screenshot.exists(),
        "returncode": completed.returncode,
        "screenshot": str(screenshot),
        "screenshot_exists": screenshot.exists(),
        "stdout": completed.stdout.strip(),
        "stderr": completed.stderr.strip()[-2000:],
    }
    try:
        payload["smoke"] = json.loads(completed.stdout.strip().splitlines()[-1])
    except (IndexError, json.JSONDecodeError):
        payload["smoke"] = None
    return payload


def run_selfcheck(
    *,
    samples_root: str | Path = "samples",
    artifacts_root: str | Path = "artifacts",
    include_ui: bool = True,
) -> dict[str, Any]:
    project_root = Path.cwd()
    samples_path = Path(samples_root)
    artifacts_path = Path(artifacts_root)
    artifacts_path.mkdir(parents=True, exist_ok=True)
    snapshot_root = artifacts_path / "snapshots"
    snapshot_root.mkdir(parents=True, exist_ok=True)

    index = generate_samples(samples_path)
    bundle_map = {
        entry["device_id"]: load_sample(samples_path / entry["path"])
        for entry in index["samples"]
    }
    checks: list[dict[str, Any]] = []

    def sample_validation() -> dict[str, Any]:
        results = {}
        for device_id, bundle in bundle_map.items():
            issues = []
            try:
                from .validation import validate_bundle

                issues = validate_bundle(bundle)
            except StudioError as exc:
                issues = [exc.to_issue()]
            results[device_id] = {
                "issues": [issue.to_dict() for issue in issues],
                "event_count": len(bundle.events),
                "axis_count": len(bundle.device.axes),
                "button_count": len(bundle.device.buttons),
                "automation_repeats": sum(
                    1
                    for event in bundle.events
                    if event.type == "button" and event.repeat
                ),
            }
        return {
            "passed": all(not item["issues"] for item in results.values()),
            "samples": results,
        }

    _run_check(checks, "multiple device sample validation", sample_validation)

    dual_path = samples_path / "dual_stick"
    for code, mutator in (
        ("E_SCHEMA", _mutate_missing_field),
        ("E_AXIS", _mutate_unknown_axis),
        ("E_ORDER", _mutate_timestamp_order),
        ("E_PARA", _mutate_bad_window),
    ):
        def validation_case(
            expected_code: str = code,
            mutation: Callable[[Path], None] = mutator,
        ) -> dict[str, Any]:
            result = _expect_validation_code(dual_path, mutation)
            result["passed"] = result["code"] == expected_code
            return result

        _run_check(
            checks,
            f"validation error {code}",
            validation_case,
        )

    def truth_checks() -> dict[str, Any]:
        details = {}
        for device_id, bundle in bundle_map.items():
            result = replay_full(bundle)
            truth = check_truth(bundle, result)
            details[device_id] = {
                "passed": truth.passed,
                "checks": len(truth.checks),
                "failures": [failure.to_dict() for failure in truth.failures],
                "frames": len(result.frames),
                "result_hash": result.result_hash,
            }
        return {
            "passed": all(item["passed"] for item in details.values()),
            "samples": details,
        }

    _run_check(checks, "deterministic replay vs truth manifests", truth_checks)

    incremental_details: dict[str, Any] = {}

    def incremental_checks() -> dict[str, Any]:
        bundle = bundle_map["dual_stick"]
        pipeline = Pipeline(bundle, max_event_buffer=128)
        initial = pipeline.recompute()
        fresh_initial = Pipeline(bundle, max_event_buffer=128).recompute()
        initial_equal = compare_results(initial.result, fresh_initial.result).equal

        device = bundle.device.with_axis("left_x", deadzone=0.20)
        run_deadzone = pipeline.update(device=device)
        fresh_deadzone = Pipeline(
            replace(bundle, device=device),
            max_event_buffer=128,
        ).recompute()
        deadzone_diff = compare_results(run_deadzone.result, fresh_deadzone.result)
        deadzone_invalidated = [
            entry.node_id for entry in run_deadzone.invalidations if entry.invalidated
        ]

        bindings = pipeline.bindings.with_rule(
            "move_x",
            curve=CurveSpec(kind="power", exponent=2.0),
        )
        run_rule = pipeline.update(bindings=bindings)
        fresh_rule = Pipeline(
            replace(pipeline.bundle, bindings=bindings),
            max_event_buffer=128,
        ).recompute()
        rule_diff = compare_results(run_rule.result, fresh_rule.result)
        rule_invalidated = [
            entry.node_id for entry in run_rule.invalidations if entry.invalidated
        ]

        timing_bindings = pipeline.bindings.with_rule("sprint", window_ms=100)
        run_timing = pipeline.update(bindings=timing_bindings)
        fresh_timing = Pipeline(
            replace(pipeline.bundle, bindings=timing_bindings),
            max_event_buffer=128,
        ).recompute()
        timing_diff = compare_results(run_timing.result, fresh_timing.result)
        timing_invalidated = [
            entry.node_id for entry in run_timing.invalidations if entry.invalidated
        ]
        details = {
            "passed": (
                initial_equal
                and deadzone_diff.equal
                and rule_diff.equal
                and timing_diff.equal
                and "axis:left_y" in run_deadzone.reused_node_ids
                and "rule:jump" in run_deadzone.reused_node_ids
                and "axis:left_x" in deadzone_invalidated
                and "rule:move_x" in deadzone_invalidated
                and not any(node.startswith("axis:") for node in rule_invalidated)
            ),
            "initial": {
                "equal": initial_equal,
                "recomputed": initial.recomputed_count,
                "reused": initial.reused_count,
                "result_hash": initial.result.result_hash,
            },
            "deadzone_change": {
                "equal": deadzone_diff.equal,
                "recomputed": run_deadzone.recomputed_count,
                "reused": run_deadzone.reused_count,
                "invalidated_nodes": deadzone_invalidated,
                "affected_windows": sorted(
                    {
                        window
                        for entry in run_deadzone.invalidations
                        for window in entry.window_ids
                    }
                ),
            },
            "rule_curve_change": {
                "equal": rule_diff.equal,
                "recomputed": run_rule.recomputed_count,
                "reused": run_rule.reused_count,
                "invalidated_nodes": rule_invalidated,
            },
            "timing_change": {
                "equal": timing_diff.equal,
                "recomputed": run_timing.recomputed_count,
                "reused": run_timing.reused_count,
                "invalidated_nodes": timing_invalidated,
            },
        }
        incremental_details.update(details)
        return details

    _run_check(checks, "incremental replay equals fresh full replay", incremental_checks)

    def divergence_check() -> dict[str, Any]:
        bundle = bundle_map["dual_stick"]
        result = replay_full(bundle)
        original_frame = result.frames[0]
        action_id = result.action_ids[0]
        snapshot = original_frame.actions[action_id]
        changed_snapshot = replace(snapshot, value=snapshot.value + 0.25)
        changed_frame = replace(
            original_frame,
            actions={**original_frame.actions, action_id: changed_snapshot},
        )
        changed_result = replace(
            result,
            frames=(changed_frame,) + result.frames[1:],
        )
        diff = compare_results(changed_result, result)
        code = None
        try:
            compare_results(changed_result, result, raise_on_divergence=True)
        except DivergenceError as exc:
            code = exc.code
        return {
            "passed": not diff.equal and code == "E_DIVERGE",
            "equal": diff.equal,
            "first_frame_index": diff.first_frame_index,
            "event_seq": diff.event_seq,
            "action": diff.action,
            "field": diff.field,
            "exception_code": code,
        }

    _run_check(checks, "divergence localization returns E_DIVERGE", divergence_check)

    snapshot_details: dict[str, Any] = {}

    def snapshot_checks() -> dict[str, Any]:
        bundle = bundle_map["dual_stick"]
        original = Pipeline(bundle, max_event_buffer=128)
        original_run = original.recompute()
        store = SnapshotStore(snapshot_root / "selfcheck_valid.json")
        write_report = SerializedSnapshotWriter(store).write(original)
        restored_pipeline = Pipeline(bundle, max_event_buffer=128)
        load_result = store.load(restored_pipeline)
        restored_run = None
        if load_result.valid:
            restored_pipeline.install_cache_entries(load_result.entries)
            restored_run = restored_pipeline.recompute()
        mismatch_pipeline = Pipeline(
            bundle_map["single_stick"],
            max_event_buffer=128,
        )
        mismatch = store.load(mismatch_pipeline)

        serial_store = SnapshotStore(snapshot_root / "selfcheck_serial.json")
        serial_writer = SerializedSnapshotWriter(serial_store)
        with ThreadPoolExecutor(max_workers=4) as executor:
            serial_reports = list(
                executor.map(lambda _: serial_writer.write(original), range(4))
            )
        serial_pipeline = Pipeline(bundle, max_event_buffer=128)
        serial_load = serial_store.load(serial_pipeline)
        details = {
            "passed": (
                load_result.valid
                and restored_run is not None
                and restored_run.recomputed_count == 0
                and restored_run.result.result_hash == original_run.result.result_hash
                and not mismatch.valid
                and bool(mismatch.invalidated_node_count)
                and serial_load.valid
                and serial_writer.write_count == 4
            ),
            "write": write_report.to_dict(),
            "load": load_result.to_dict(),
            "restored_recomputed": restored_run.recomputed_count if restored_run else None,
            "restored_reused": restored_run.reused_count if restored_run else None,
            "restored_hash": restored_run.result.result_hash if restored_run else None,
            "mismatch": mismatch.to_dict(),
            "serialized_writes": [report.to_dict() for report in serial_reports],
            "serial_load_valid": serial_load.valid,
        }
        snapshot_details.update(details)
        return details

    _run_check(checks, "atomic snapshot save/load and version invalidation", snapshot_checks)

    hot_switch_details: dict[str, Any] = {}

    def hot_switch_checks() -> dict[str, Any]:
        source = bundle_map["dual_stick"]
        target = bundle_map["hybrid_trigger_gyro"]
        conflict_analysis = plan_hot_switch(
            source,
            target,
            target_bindings=source.bindings,
        )
        conflict_code = None
        conflict_context: dict[str, Any] = {}
        try:
            HotSwitchSession.raise_for_conflicts(conflict_analysis)
        except MappingError as exc:
            conflict_code = exc.code
            conflict_context = exc.context
        session = HotSwitchSession(
            source,
            target,
            switch_t_ms=2000,
            max_event_buffer=128,
        )
        prefix_before = session.result.verify_prefix()
        session.update_target(
            device=target.device.with_axis("stick_x", deadzone=0.22)
        )
        prefix_after = session.result.verify_prefix()
        direct = session.verify_direct_target()
        details = {
            "passed": (
                conflict_code == "E_MAP"
                and conflict_context.get("source_device") == "dual_stick"
                and conflict_context.get("target_device") == "hybrid_trigger_gyro"
                and bool(conflict_context.get("axis_id"))
                and session.result.direct_match
                and direct.get("equal")
                and prefix_before
                and prefix_after
            ),
            "conflict_code": conflict_code,
            "conflict_context": conflict_context,
            "unmapped_inputs": [
                item.to_dict() for item in session.result.analysis.unmapped
            ],
            "migration_notes": list(session.result.analysis.migration_notes),
            "prefix_frames": len(session.result.prefix_frames),
            "suffix_frames": len(session.result.suffix_frames),
            "prefix_hash": session.result.prefix_hash,
            "prefix_unchanged_after_target_update": prefix_before and prefix_after,
            "direct_match": session.result.direct_match,
            "direct_diff": direct,
        }
        hot_switch_details.update(details)
        return details

    _run_check(checks, "hot-switch migration and conflict reporting", hot_switch_checks)

    def concurrency_checks() -> dict[str, Any]:
        with ReplayCoordinator(max_parallel=2, event_buffer_limit=128) as coordinator:
            report = coordinator.parallel_determinism_check(
                bundle_map["dual_stick"],
                runs=3,
            )
        return {
            "passed": report.all_equal,
            **report.to_dict(),
        }

    _run_check(checks, "bounded parallel replay determinism", concurrency_checks)

    if include_ui:
        screenshot = artifacts_path / "ui_dashboard.png"
        _run_check(
            checks,
            "PySide6 dashboard smoke render",
            lambda: _run_ui_smoke(project_root, screenshot),
        )

    summary = {
        "passed": all(check["passed"] for check in checks),
        "check_count": len(checks),
        "failed_count": sum(1 for check in checks if not check["passed"]),
    }
    report = {
        "schema_version": 1,
        "generated_at_utc": datetime.now(timezone.utc).isoformat(),
        "environment": {
            "python": sys.version,
            "numpy": numpy.__version__,
            "pyside6": PySide6.__version__,
        },
        "samples_index": index,
        "summary": summary,
        "checks": checks,
        "incremental": incremental_details,
        "snapshot": snapshot_details,
        "hot_switch": hot_switch_details,
        "notes": [
            "All timestamps are logical milliseconds.",
            "Timing window boundaries are inclusive: deadline ticks run before a "
            "release at the long-press threshold and after a completing event at a "
            "combo/double-press window boundary.",
            "Snapshots are atomically replaced and validated as a whole. A forced "
            "termination during a write leaves the previous complete snapshot or no "
            "replacement; no partial-snapshot success is claimed.",
        ],
    }
    report_path = artifacts_path / "validation_report.json"
    write_sample_json(report_path, report)
    markdown_lines = [
        "# PadWeave validation report",
        "",
        f"- Result: **{'PASS' if summary['passed'] else 'FAIL'}**",
        f"- Checks: {summary['check_count']} total, {summary['failed_count']} failed",
        f"- Python: {sys.version.split()[0]}",
        f"- NumPy: {numpy.__version__}",
        f"- PySide6: {PySide6.__version__}",
        "",
        "## Checks",
        "",
    ]
    for check in checks:
        markdown_lines.append(
            f"- [{'x' if check['passed'] else ' '}] {check['name']}"
        )
    markdown_lines.extend(
        [
            "",
            "## Incremental evidence",
            "",
            "```json",
            json.dumps(_json_safe(incremental_details), ensure_ascii=False, indent=2),
            "```",
            "",
        ]
    )
    (artifacts_path / "validation_summary.md").write_text(
        "\n".join(markdown_lines),
        encoding="utf-8",
    )
    return report


__all__ = ["run_selfcheck"]

