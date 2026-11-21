"""End-to-end local acceptance checks with machine-readable evidence."""

from __future__ import annotations

import copy
import platform
import shutil
import sys
import time
from pathlib import Path
from typing import Any

from .baseline import compare_strategies, compare_with_baseline_file
from .config import load_config
from .engine import ReplayEngine
from .errors import BusinessError, RecoveryInvalidError
from .generator import generate_samples
from .model import load_record
from .snapshot import SnapshotStore
from .util import atomic_write_json, load_json, tree_digest


def _check(report: dict[str, Any], name: str, passed: bool, **details: Any) -> None:
    report["checks"][name] = {"ok": passed, **details}
    if not passed:
        report["ok"] = False


def _safe_clean(path: Path) -> None:
    if path.exists():
        shutil.rmtree(path)
    path.mkdir(parents=True, exist_ok=True)


def _result_bytes(
    record_path: Path,
    config_path: Path,
    snapshots: Path | None = None,
    *,
    resume: bool = False,
    stop_after: int | None = None,
) -> tuple[bytes, Any, Any]:
    config = load_config(config_path)
    record = load_record(record_path, config)
    engine = ReplayEngine(
        record,
        config,
        snapshot_dir=snapshots,
        resume=resume,
    )
    run = engine.run(stop_after_step=stop_after)
    return run.to_bytes(), run, engine


def run_acceptance(out_dir: str | Path, *, seed: int = 20261122) -> dict[str, Any]:
    started = time.perf_counter()
    root = Path(out_dir)
    root.mkdir(parents=True, exist_ok=True)
    samples_dir = root / "samples"
    repeat_dir = root / "samples-repeat"
    _safe_clean(samples_dir)
    _safe_clean(repeat_dir)
    profiles = ["small", "medium", "stress", "conflict"]
    manifest = generate_samples(samples_dir, seed=seed, profiles=profiles)
    repeat_manifest = generate_samples(repeat_dir, seed=seed, profiles=profiles)
    report: dict[str, Any] = {
        "ok": True,
        "seed": seed,
        "environment": _environment(),
        "checks": {},
        "profiles": {},
    }
    _check(
        report,
        "deterministic_generation",
        manifest == repeat_manifest
        and tree_digest(samples_dir) == tree_digest(repeat_dir),
        digest=tree_digest(samples_dir),
    )

    for profile in ("small", "medium", "stress"):
        sample = samples_dir / profile
        record_path = sample / "match.json"
        config_path = sample / "config.json"
        baseline_path = sample / "baseline.json"
        direct_bytes, run, _ = _result_bytes(record_path, config_path)
        profile_report: dict[str, Any] = {
            "strategy": run.strategy,
            "steps": len(run.frames),
            "final_state_hash": run.final_state_hash,
            "result_bytes": len(direct_bytes),
        }
        comparison = compare_with_baseline_file(run, baseline_path)
        profile_report["baseline"] = comparison
        _check(
            report,
            f"{profile}_baseline",
            comparison["ok"],
            steps=comparison["steps_compared"],
        )

        config = load_config(config_path)
        record = load_record(record_path, config)
        strategy_result = compare_strategies(record, config)
        profile_report["strategy_check"] = strategy_result
        _check(
            report,
            f"{profile}_strategy_equivalence",
            strategy_result["ok"],
            digest=strategy_result["full_scan_digest"],
        )

        snapshot_dir = root / "snapshots" / profile
        _safe_clean(snapshot_dir)
        stop_step = max(1, len(run.frames) // 2)
        partial_bytes, partial_run, _ = _result_bytes(
            record_path,
            config_path,
            snapshot_dir,
            stop_after=stop_step,
        )
        profile_report["interrupted"] = {
            "step": stop_step,
            "complete": partial_run.complete,
            "snapshots": partial_run.latest_snapshot_step,
        }
        _check(
            report,
            f"{profile}_interrupt_has_snapshots",
            (not partial_run.complete)
            and partial_run.latest_snapshot_step == stop_step,
            snapshot_step=partial_run.latest_snapshot_step,
        )
        resumed_bytes, resumed_run, _ = _result_bytes(
            record_path,
            config_path,
            snapshot_dir,
            resume=True,
        )
        profile_report["resume_byte_identical"] = resumed_bytes == direct_bytes
        _check(
            report,
            f"{profile}_resume_byte_identical",
            resumed_run.complete and resumed_bytes == direct_bytes,
            resumed_steps=len(resumed_run.frames),
            digest=tree_digest(snapshot_dir),
        )

        baseline = load_json(baseline_path)
        altered = copy.deepcopy(baseline)
        target_index = min(2, len(altered["frames"]) - 1)
        altered["frames"][target_index]["state_hash"] = "0" * 64
        divergent_path = root / "diagnostics" / f"{profile}-divergent-baseline.json"
        atomic_write_json(divergent_path, altered)
        try:
            compare_with_baseline_file(run, divergent_path)
            divergence = None
        except BusinessError as error:
            divergence = error.to_dict()
        _check(
            report,
            f"{profile}_first_divergence",
            divergence is not None
            and divergence["code"] == "E_BASELINE_DIVERGENCE"
            and divergence["step"] == altered["frames"][target_index]["step"]
            and "state_hash" in divergence["field_path"],
            divergence=divergence,
        )
        report["profiles"][profile] = profile_report

    conflict_sample = samples_dir / "conflict"
    config = load_config(conflict_sample / "config.json")
    record = load_record(conflict_sample / "match.json", config)
    conflict_run = ReplayEngine(
        record, config, allow_conflicts=True
    ).run()
    conflict_codes = [item.code for item in conflict_run.conflicts]
    _check(
        report,
        "credit_conflict_detection",
        "E_CREDIT_CONFLICT" in conflict_codes,
        conflict=conflict_run.conflicts[0].to_dict() if conflict_run.conflicts else None,
    )

    recovery_dir = root / "snapshots" / "corruption"
    _safe_clean(recovery_dir)
    record_path = samples_dir / "small" / "match.json"
    config_path = samples_dir / "small" / "config.json"
    _, _, engine = _result_bytes(
        record_path, config_path, recovery_dir, stop_after=4
    )
    corrupt_path = recovery_dir / "step-000003.json"
    corrupted = load_json(corrupt_path)
    corrupted["frame"]["state_hash"] = "f" * 64
    atomic_write_json(corrupt_path, corrupted)
    try:
        SnapshotStore(
            recovery_dir,
            input_digest=engine.input_digest,
            config=engine.config,
        ).load_latest()
        recovery_error: dict[str, Any] | None = None
    except BusinessError as error:
        recovery_error = error.to_dict()
    _check(
        report,
        "recovery_chain_break",
        recovery_error is not None
        and recovery_error["code"] == "E_RECOVERY_INVALID"
        and recovery_error["first_broken_step"] == 3,
        error=recovery_error,
    )
    report["duration_seconds"] = round(time.perf_counter() - started, 3)
    report["python_ok"] = sys.version_info >= (3, 11)
    atomic_write_json(root / "acceptance-report.json", report)
    return report


def _environment() -> dict[str, Any]:
    payload: dict[str, Any] = {
        "python": platform.python_version(),
        "platform": platform.platform(),
    }
    try:
        import numpy

        payload["numpy"] = numpy.__version__
    except ImportError:
        payload["numpy"] = None
    try:
        import PySide6

        payload["pyside6"] = PySide6.__version__
    except ImportError:
        payload["pyside6"] = None
    return payload
