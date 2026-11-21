"""End-to-end local verification runner.

This module produces a deterministic report.  It never uses network access,
wall-clock values, or file sizes as substitutes for content hashes.
"""

from __future__ import annotations

import copy
import hashlib
import json
import os
from typing import Any, Callable

from .baseline import compare_baseline
from .compare import compare_paths
from .engine import ReplayEngine, run_recording
from .errors import ReplayError
from .generator import generate_samples
from .injection import truncate_latest_snapshot
from .lock import MatchLock
from .model import serialization_order
from .snapshot import SnapshotStore
from .trajectory import trajectory_bytes, write_trajectory
from .validation import atomic_write_json, load_recording


def _error_snapshot(call: Callable[[], Any]) -> dict[str, Any]:
    try:
        call()
    except ReplayError as exc:
        return exc.to_dict()
    return {"ok": True, "expected_error": False}


def _sanitize_paths(value: Any, root: str) -> Any:
    if isinstance(value, str) and os.path.isabs(value):
        try:
            relative = os.path.relpath(value, root)
        except ValueError:
            return value
        if not relative.startswith(".."):
            return relative
        return value
    if isinstance(value, dict):
        return {key: _sanitize_paths(item, root) for key, item in value.items()}
    if isinstance(value, list):
        return [_sanitize_paths(item, root) for item in value]
    return value


def _run_case(
    sample_dir: str,
    case: dict[str, Any],
    case_out: str,
) -> dict[str, Any]:
    recording_path = os.path.join(sample_dir, case["recording"])
    baseline_path = os.path.join(sample_dir, case["baseline"])
    mismatch_path = os.path.join(sample_dir, case["baseline_mismatch"])
    recording = load_recording(recording_path)
    os.makedirs(case_out, exist_ok=True)

    fixed = run_recording(recording, "fixed")
    floating = run_recording(recording, "float")
    fixed_meta = write_trajectory(fixed, os.path.join(case_out, "trajectory-fixed.jsonl"))
    float_meta = write_trajectory(floating, os.path.join(case_out, "trajectory-float.jsonl"))

    rerun_fixed = run_recording(recording, "fixed")
    rerun_float = run_recording(recording, "float")
    fixed_bytes = trajectory_bytes(fixed)
    float_bytes = trajectory_bytes(floating)
    determinism_per_mode = {
        "fixed": {
            "byte_identical": fixed_bytes == trajectory_bytes(rerun_fixed),
            "state_hash_identical": fixed.final_state_hash == rerun_fixed.final_state_hash,
            "trajectory_sha256": hashlib.sha256(fixed_bytes).hexdigest(),
        },
        "float": {
            "byte_identical": float_bytes == trajectory_bytes(rerun_float),
            "state_hash_identical": floating.final_state_hash == rerun_float.final_state_hash,
            "trajectory_sha256": hashlib.sha256(float_bytes).hexdigest(),
        },
    }

    dual = compare_paths(recording, fixed_run=fixed, float_run=floating)
    atomic_write_json(os.path.join(case_out, "dual-report.json"), dual)
    baseline_result = compare_baseline(fixed, baseline_path)
    atomic_write_json(os.path.join(case_out, "baseline-report.json"), baseline_result)
    mismatch_result = compare_baseline(fixed, mismatch_path)
    atomic_write_json(
        os.path.join(case_out, "baseline-mismatch-report.json"),
        mismatch_result,
    )

    total_steps = recording.config.total_steps
    resume_at = min(7, max(1, total_steps // 2))
    snapshot_dir = os.path.join(case_out, "snapshots")
    store = SnapshotStore(
        snapshot_dir,
        match_id=recording.match_id,
        mode="fixed",
        config_digest=recording.config_digest(),
    )
    partial = run_recording(
        recording,
        "fixed",
        snapshot_store=store,
        cancel_after=resume_at,
    )
    snapshot = store.load_latest()
    if snapshot is None:
        raise AssertionError("partial run did not commit snapshots")
    recovered = ReplayEngine(
        recording,
        "fixed",
        snapshot_store=store,
    ).resume(snapshot)
    recovered_meta = write_trajectory(
        recovered,
        os.path.join(case_out, "trajectory-recovered.jsonl"),
    )
    recovery_ok = (
        recovered.final_state_hash == fixed.final_state_hash
        and recovered.final_chain_hash == fixed.final_chain_hash
        and recovered.last_committed_step == total_steps
        and (not recovered.steps or recovered.steps[0].step == resume_at + 1)
        and partial.canceled
        and partial.last_committed_step == resume_at
    )
    recovery_report = {
        "ok": recovery_ok,
        "cancel_after": resume_at,
        "partial_last_committed_step": partial.last_committed_step,
        "partial_canceled": partial.canceled,
        "snapshot_step": snapshot.step,
        "snapshot_state_hash": snapshot.state_hash,
        "resumed_from": recovered.resumed_from,
        "first_replayed_step": recovered.steps[0].step if recovered.steps else None,
        "final_state_hash": recovered.final_state_hash,
        "expected_final_state_hash": fixed.final_state_hash,
        "final_chain_hash": recovered.final_chain_hash,
        "expected_final_chain_hash": fixed.final_chain_hash,
        "recovered_trajectory": recovered_meta,
    }
    atomic_write_json(os.path.join(case_out, "recovery-report.json"), recovery_report)

    trunc_dir = os.path.join(case_out, "snapshots-truncated")
    trunc_store = SnapshotStore(
        trunc_dir,
        match_id=recording.match_id,
        mode="fixed",
        config_digest=recording.config_digest(),
    )
    run_recording(
        recording,
        "fixed",
        snapshot_store=trunc_store,
        cancel_after=min(5, resume_at),
    )
    truncation_context = truncate_latest_snapshot(trunc_dir)
    truncated_error = _error_snapshot(trunc_store.load_latest)

    untrusted_entry = next(
        (
            entry
            for step in fixed.steps
            for entry in step.untrusted
        ),
        None,
    )
    cross_mode_hash_equal_steps = sum(
        1
        for left, right in zip(fixed.steps, floating.steps)
        if left.state_hash == right.state_hash
    )
    return {
        "name": case["name"],
        "match_id": case["match_id"],
        "source_version": recording.source_version,
        "derived_fields": recording.derived_fields,
        "fixed_trajectory": fixed_meta,
        "float_trajectory": float_meta,
        "determinism": determinism_per_mode,
        "dual_path": {
            "deterministic_fixed": dual["deterministic_fixed"],
            "deterministic_float": dual["deterministic_float"],
            "input_semantics_sha256": dual["input_semantics_sha256"],
            "fixed_final_state_hash": dual["fixed_final_state_hash"],
            "float_final_state_hash": dual["float_final_state_hash"],
            "deviation_count": len(dual["deviations"]),
            "tolerance_exceeded_steps": dual["tolerance_exceeded_steps"],
            "first_divergence": dual["first_divergence"],
            "untrusted_steps": dual["untrusted_steps"],
            "first_attribution": untrusted_entry,
            "cross_mode_hash_equal_steps": cross_mode_hash_equal_steps,
            "total_steps": total_steps,
        },
        "baseline": {
            "ok": baseline_result["ok"],
            "steps_compared": baseline_result["steps_compared"],
            "first_mismatch": baseline_result["first_mismatch"],
        },
        "baseline_mismatch": {
            "ok": mismatch_result["ok"],
            "first_mismatch": mismatch_result["first_mismatch"],
        },
        "recovery": recovery_report,
        "truncated_snapshot": {
            "injection": truncation_context,
            "error": truncated_error,
        },
        "sample_paths": {
            "recording": case["recording"],
            "baseline": case["baseline"],
            "baseline_mismatch": case["baseline_mismatch"],
        },
    }


def _recording_injections(sample_dir: str, case: dict[str, Any]) -> list[dict[str, Any]]:
    path = os.path.join(sample_dir, case["recording"])
    result: list[dict[str, Any]] = []
    for name in (
        "dup_client_seq",
        "out_of_bounds_coord",
        "nonfinite_heading",
        "late_operation",
    ):
        result.append(
            {
                "name": name,
                "error": _error_snapshot(
                    lambda name=name: load_recording(path, inject=[name])
                ),
            }
        )
    return result


def _auxiliary_errors(
    sample_dir: str,
    case: dict[str, Any],
) -> dict[str, Any]:
    recording = load_recording(os.path.join(sample_dir, case["recording"]))
    fixed = run_recording(recording, "fixed")
    baseline_path = os.path.join(sample_dir, case["baseline"])
    with open(baseline_path, "r", encoding="utf-8") as handle:
        baseline_raw = json.load(handle)

    mode_error = _error_snapshot(lambda: ReplayEngine(recording, "quantum").run())
    corrupt_baseline = _error_snapshot(
        lambda: compare_baseline(fixed, baseline_path, inject=["corrupt_baseline"])
    )
    version_payload = copy.deepcopy(baseline_raw)
    version_payload["state_model_version"] = "state-v1"
    version_path = os.path.join(
        os.path.dirname(baseline_path),
        "_verification-baseline-version.json",
    )
    atomic_write_json(version_path, version_payload)
    version_error = _error_snapshot(lambda: compare_baseline(fixed, version_path))

    lock_dir = os.path.join(os.path.dirname(baseline_path), "_verification-locks")
    first = MatchLock(lock_dir, recording.match_id)
    first.acquire()
    try:
        busy_error = _error_snapshot(
            lambda: MatchLock(lock_dir, recording.match_id).acquire()
        )
    finally:
        first.release()
    return {
        "invalid_mode": mode_error,
        "corrupt_baseline": corrupt_baseline,
        "baseline_version_mismatch": version_error,
        "busy_lock": busy_error,
    }


def _markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Replay verification",
        "",
        f"Sample seed: `{report['seed']}`",
        "",
        "This report is generated from local deterministic samples. It contains no "
        "timestamps and no file-size based checks.",
        "",
        "## Cases",
        "",
        "| Case | Fixed final hash | Float final hash | Baseline | Recovered | Deviations | Untrusted steps |",
        "|---|---|---|---|---|---|---|",
    ]
    for case in report["cases"]:
        dual = case["dual_path"]
        lines.append(
            "| {name} | `{fixed}` | `{floating}` | {baseline} | {recovery} | "
            "{deviations} | {untrusted} |".format(
                name=case["name"],
                fixed=dual["fixed_final_state_hash"],
                floating=dual["float_final_state_hash"],
                baseline="PASS" if case["baseline"]["ok"] else "FAIL",
                recovery="PASS" if case["recovery"]["ok"] else "FAIL",
                deviations=dual["deviation_count"],
                untrusted=len(dual["untrusted_steps"]),
            )
        )
    lines.extend(
        [
            "",
            "## Determinism",
            "",
            "| Case | Fixed bytes equal | Float bytes equal | Fixed trajectory SHA-256 | Float trajectory SHA-256 |",
            "|---|---|---|---|---|",
        ]
    )
    for case in report["cases"]:
        det = case["determinism"]
        lines.append(
            "| {name} | {fixed} | {floating} | `{fixed_hash}` | `{float_hash}` |".format(
                name=case["name"],
                fixed=det["fixed"]["byte_identical"],
                floating=det["float"]["byte_identical"],
                fixed_hash=det["fixed"]["trajectory_sha256"],
                float_hash=det["float"]["trajectory_sha256"],
            )
        )
    lines.extend(
        [
            "",
            "## Anomaly channels",
            "",
            "| Channel | Business code | Location |",
            "|---|---|---|",
        ]
    )
    for item in report["injections"]:
        error = item["error"]
        lines.append(
            "| {name} | `{code}` | `{path}` |".format(
                name=item["name"],
                code=error.get("code", "UNEXPECTED_SUCCESS"),
                path=error.get("path", error.get("injection", {}).get("point", "")),
            )
        )
    aux = report["auxiliary_errors"]
    for name, error in aux.items():
        lines.append(
            "| {name} | `{code}` | `{path}` |".format(
                name=name,
                code=error.get("code", "UNEXPECTED_SUCCESS"),
                path=error.get("path", ""),
            )
        )
    lines.extend(
        [
            "",
            "## Summary",
            "",
        ]
    )
    for key in sorted(report["summary"]):
        lines.append(f"- `{key}`: `{report['summary'][key]}`")
    lines.append("")
    return "\n".join(lines)


def verify_all(out_dir: str, *, seed: int = 20261122) -> dict[str, Any]:
    out_dir = os.path.abspath(out_dir)
    sample_dir = os.path.join(out_dir, "samples")
    manifest = generate_samples(sample_dir, seed=seed)
    cases_report: list[dict[str, Any]] = []
    for case in manifest["cases"]:
        case_out = os.path.join(sample_dir, case["name"], "verification")
        cases_report.append(_run_case(sample_dir, case, case_out))

    first_case = manifest["cases"][0]
    injections = _recording_injections(sample_dir, first_case)
    injection_ok = all(
        item["error"].get("code") not in (None, "UNEXPECTED_SUCCESS")
        for item in injections
    )
    auxiliary = _auxiliary_errors(sample_dir, first_case)
    all_aux_ok = all(
        error.get("code") not in (None, "UNEXPECTED_SUCCESS")
        for error in auxiliary.values()
    )
    baseline_ok = all(case["baseline"]["ok"] for case in cases_report)
    mismatch_ok = all(not case["baseline_mismatch"]["ok"] for case in cases_report)
    recovery_ok = all(case["recovery"]["ok"] for case in cases_report)
    determinism_ok = all(
        case["determinism"]["fixed"]["byte_identical"]
        and case["determinism"]["float"]["byte_identical"]
        and case["dual_path"]["deterministic_fixed"]
        and case["dual_path"]["deterministic_float"]
        for case in cases_report
    )
    truncation_ok = all(
        case["truncated_snapshot"]["error"].get("code") == "E_RECOVERY_INVALID"
        for case in cases_report
    )
    exact_case = next(
        (case for case in cases_report if case["name"] == "case-04-exact-dual"),
        None,
    )
    exact_cross_mode = bool(
        exact_case
        and exact_case["dual_path"]["cross_mode_hash_equal_steps"]
        == exact_case["dual_path"]["total_steps"]
    )
    summary = {
        "ok": all(
            (
                baseline_ok,
                mismatch_ok,
                recovery_ok,
                determinism_ok,
                truncation_ok,
                injection_ok,
                all_aux_ok,
                exact_cross_mode,
            )
        ),
        "cases": len(cases_report),
        "baseline_match_all": baseline_ok,
        "baseline_mismatch_detected_all": mismatch_ok,
        "recovery_ok_all": recovery_ok,
        "determinism_ok_all": determinism_ok,
        "truncated_snapshot_rejected_all": truncation_ok,
        "recording_injections_ok": injection_ok,
        "auxiliary_errors_ok": all_aux_ok,
        "exact_case_cross_mode_hashes_equal": exact_cross_mode,
        "serialization_order": serialization_order(),
    }
    report = _sanitize_paths({
        "verification_version": "verification-v1",
        "seed": seed,
        "sample_manifest": manifest,
        "cases": cases_report,
        "injections": injections,
        "auxiliary_errors": auxiliary,
        "summary": summary,
    }, out_dir)
    atomic_write_json(os.path.join(out_dir, "verification.json"), report)
    with open(os.path.join(out_dir, "verification.md"), "w", encoding="utf-8") as handle:
        handle.write(_markdown(report))
    return report
