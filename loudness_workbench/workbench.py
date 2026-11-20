"""End-to-end run, offline comparison, snapshots, and result reporting."""

from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path
from typing import Any

from .config import WorkbenchConfig
from .errors import ErrorCode, MeterError, error
from .generator import generate_samples
from .metering import AssetMeter, analyze_offline, compare_reports
from .models import AssetReport
from .scheduler import MeterScheduler
from .snapshot import SnapshotStore


def _atomic_json(path: str | Path, payload: Any) -> Path:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=str(path.parent),
    )
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as handle:
            json.dump(
                payload,
                handle,
                sort_keys=True,
                indent=2,
                ensure_ascii=False,
                allow_nan=False,
            )
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise
    return path


def _snapshot_store(config: WorkbenchConfig, asset_id: str) -> SnapshotStore:
    safe = "".join(
        character if character.isalnum() or character in {"-", "_"} else "_"
        for character in asset_id
    )
    return SnapshotStore(Path(config.snapshot_dir) / f"{safe}.json")


def _load_truth(path: Path) -> dict[str, dict[str, Any]]:
    if not path.exists():
        return {}
    raw = json.loads(path.read_text(encoding="utf-8"))
    return {
        str(item["asset_id"]): item
        for item in raw.get("assets", [])
        if isinstance(item, dict) and "asset_id" in item
    }


def run_workbench(
    config: WorkbenchConfig,
    *,
    run_offline_check: bool = True,
    write_result: bool = True,
) -> dict[str, Any]:
    """Run all configured assets through the scheduler and verify contracts."""
    config.validate()
    artifact_dir = Path(config.artifact_dir)
    artifact_dir.mkdir(parents=True, exist_ok=True)
    meters: dict[str, AssetMeter] = {}
    try:
        for asset in config.assets:
            asset_path = Path(asset.path)
            if not asset_path.exists():
                raise error(
                    ErrorCode.E_AUDIO_FORMAT,
                    "configured WAV does not exist",
                    {"asset_id": asset.asset_id, "path": str(asset_path)},
                )
            meters[asset.asset_id] = AssetMeter(
                str(asset_path),
                config=config.meter,
                asset_id=asset.asset_id,
                enforce_budget=False,
            )
    except MeterError as exc:
        result = {
            "ok": False,
            "stage": "load",
            "error": exc.to_dict(),
        }
        if write_result:
            _atomic_json(artifact_dir / "result.json", result)
        return result

    def on_block_complete(meter: AssetMeter, block: Any) -> None:
        _snapshot_store(config, meter.asset_id).save(meter.get_published())

    scheduler = MeterScheduler(
        config.meter,
        trace_path=config.trace_path,
        run_metadata={
            "seed": config.seed,
            "asset_ids": [asset.asset_id for asset in config.assets],
            "block_samples": config.meter.block_samples,
        },
        on_block_complete=on_block_complete,
    )
    try:
        for asset in config.assets:
            scheduler.start_asset(asset.asset_id, meters[asset.asset_id])
        schedule_result = scheduler.execute()
    except MeterError as exc:
        result = {
            "ok": False,
            "stage": "schedule",
            "error": exc.to_dict(),
            "published": {
                asset_id: meter.get_published().to_dict()
                for asset_id, meter in meters.items()
            },
        }
        if write_result:
            _atomic_json(artifact_dir / "result.json", result)
        return result

    comparisons: dict[str, Any] = {}
    offline_reports: dict[str, AssetReport] = {}
    if run_offline_check and schedule_result.ok:
        try:
            for asset in config.assets:
                offline = analyze_offline(
                    asset.path,
                    config=config.meter,
                    asset_id=asset.asset_id,
                )
                offline_reports[asset.asset_id] = offline
                comparisons[asset.asset_id] = compare_reports(
                    meters[asset.asset_id].get_published(),
                    offline,
                    config.meter,
                )
                _atomic_json(
                    artifact_dir / f"offline_{asset.asset_id}.json",
                    offline.to_dict(),
                )
        except MeterError as exc:
            schedule_result = type(schedule_result)(
                ok=False,
                completions=schedule_result.completions,
                errors=schedule_result.errors + (exc.to_dict(),),
                degraded_blocks=schedule_result.degraded_blocks,
                waiting_events=schedule_result.waiting_events,
                trace_events=schedule_result.trace_events,
            )

    snapshot_paths: dict[str, str] = {}
    final_reports: dict[str, AssetReport] = {}
    if schedule_result.ok:
        for asset in config.assets:
            meter = meters[asset.asset_id]
            report = meter.get_published()
            if report.complete:
                store = _snapshot_store(config, asset.asset_id)
                store.save(report)
                recovered = store.load(
                    expected_parameter_version=report.parameter_version,
                    expected_config_hash=report.config_hash,
                )
                if recovered.report_hash != report.report_hash:
                    raise MeterError(
                        ErrorCode.E_REPORT_MISMATCH,
                        "snapshot recovery returned a different report",
                        {"asset_id": asset.asset_id},
                    )
                snapshot_paths[asset.asset_id] = str(store.path)
                _atomic_json(
                    artifact_dir / f"report_{asset.asset_id}.json",
                    report.to_dict(),
                )
                final_reports[asset.asset_id] = recovered

    truth = _load_truth(Path(config.sample_dir) / "truth.json")
    truth_checks: dict[str, Any] = {}
    for asset_id, report in final_reports.items():
        declared = truth.get(asset_id)
        if not declared:
            truth_checks[asset_id] = {"available": False}
            continue
        measured_lufs = report.final_integrated_lufs
        measured_true_peak = max(
            (block.true_peak_dbtp for block in report.blocks),
            default=-120.0,
        )
        truth_checks[asset_id] = {
            "available": True,
            "declared_lufs": float(declared["declared_lufs"]),
            "measured_lufs": measured_lufs,
            "lufs_delta": measured_lufs - float(declared["declared_lufs"]),
            "declared_true_peak_dbtp": float(
                declared["declared_true_peak_dbtp"]
            ),
            "measured_true_peak_dbtp": measured_true_peak,
            "true_peak_delta_db": measured_true_peak
            - float(declared["declared_true_peak_dbtp"]),
        }

    result = {
        "ok": bool(schedule_result.ok and all(
            item.get("first_mismatch", None) is None for item in comparisons.values()
        )),
        "stage": "complete",
        "config_hash": config.meter.config_hash,
        "seed": config.seed,
        "parallelism": config.meter.scheduler.parallelism,
        "memory_budget_bytes": config.meter.scheduler.memory_budget_bytes,
        "per_task_memory_budget_bytes": (
            config.meter.scheduler.per_task_memory_budget_bytes
        ),
        "schedule": schedule_result.to_dict(),
        "comparisons": comparisons,
        "truth_checks": truth_checks,
        "assets": {
            asset_id: {
                "block_count": report.block_count,
                "complete": report.complete,
                "parameter_version": report.parameter_version,
                "final_integrated_lufs": report.final_integrated_lufs,
                "maximum_true_peak_dbtp": max(
                    (block.true_peak_dbtp for block in report.blocks),
                    default=-120.0,
                ),
                "head_hash": report.head_hash,
                "report_hash": report.report_hash,
            }
            for asset_id, report in final_reports.items()
        },
        "snapshots": snapshot_paths,
        "trace_path": config.trace_path,
    }
    if write_result:
        _atomic_json(artifact_dir / "result.json", result)
    return result


def generate_and_prepare(
    sample_dir: str | Path,
    *,
    seed: int,
    config_path: str | Path | None = None,
) -> dict[str, Any]:
    return generate_samples(sample_dir, seed=seed, config_path=config_path)
