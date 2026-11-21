#!/usr/bin/env python3
"""Run the local acceptance matrix and leave machine-readable artifacts."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tileforge.config import JobConfig, MemoryConfig
from tileforge.errors import E_BUDGET_EXCEEDED, E_VERSION_STALE, TileForgeError
from tileforge.pipeline import convert_incremental, offline_convert
from tileforge.preview import PreviewCursor, load_offline_report, preview_frame
from tileforge.samplegen import generate_samples
from tileforge.store import VersionStore


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, sort_keys=True, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")


def _trace_events(path: Path) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    if not path.exists():
        return events
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.strip():
            events.append(json.loads(line))
    return events


def main() -> int:
    output = ROOT / "artifacts" / "acceptance"
    if output.exists():
        shutil.rmtree(output)
    samples_dir = output / "samples"
    logs_dir = output / "logs"
    logs_dir.mkdir(parents=True, exist_ok=True)
    seed = 190
    sample_manifest = generate_samples(samples_dir, seed=seed)
    checks: list[dict[str, Any]] = []
    sample_results: list[dict[str, Any]] = []

    for entry in sample_manifest["samples"]:
        input_path = samples_dir / entry["sequence"]
        config = JobConfig.load(samples_dir / entry["config"])
        workspace = output / "workbenches" / entry["name"]
        summary = convert_incremental(input_path, config, workspace)
        offline_path = output / "offline" / entry["sequence"]
        offline = offline_convert(input_path, offline_path, config)
        cursor = PreviewCursor(workspace, expected_version_hash=summary.version_hash)
        cursor.refresh()
        cursor.advance_to_end()
        comparison = cursor.compare_offline(load_offline_report(offline_path.with_suffix(".tcseq.report.json")))
        manifest = VersionStore(workspace).load_current().manifest
        sample_results.append(
            {
                "name": entry["name"],
                "bit_depth": entry["header"]["bit_depth"],
                "grid": [entry["header"]["tiles_x"], entry["header"]["tiles_y"]],
                "source_transfer": entry["header"]["source_transfer"],
                "target_transfer": entry["header"]["target_transfer"],
                "version_id": summary.version_id,
                "version_hash": summary.version_hash,
                "published_file_hash": manifest["version_hash"],
                "offline_hash": offline.version_hash,
                "compared_tiles": comparison["compared_tiles"],
                "sync_marker": comparison["sync_marker"],
                "content_hashes": summary.content_hashes,
                "trace": summary.trace_path,
            }
        )
        checks.append(
            {
                "check": f"offline_preview_equal:{entry['name']}",
                "ok": comparison["ok"]
                and summary.version_hash == offline.version_hash
                and manifest["version_hash"] == offline.version_hash,
            }
        )

    incremental_name = sample_manifest["samples"][0]["name"]
    incremental_input = samples_dir / f"{incremental_name}.tcseq"
    incremental_config = JobConfig.load(samples_dir / f"{incremental_name}.config.json")
    incremental_workspace = output / "workbenches" / incremental_name
    old_current = VersionStore(incremental_workspace).load_current()
    incremental_config = replace(incremental_config, block_exposure_stops={3: 0.25})
    incremental = convert_incremental(
        incremental_input,
        incremental_config,
        incremental_workspace,
        expected_base_hash=old_current.version_hash,
    )
    checks.append(
        {
            "check": "incremental_single_tile_exposure",
            "ok": incremental.affected_tiles == [3]
            and len(incremental.reused_tiles) == len(old_current.sequence.blocks) - 1
            and incremental.version_id == old_current.version_id + 1,
            "affected_tiles": incremental.affected_tiles,
            "reused_tiles": incremental.reused_tiles,
        }
    )
    offline_incremental = offline_convert(
        incremental_input,
        output / "offline" / "incremental_equal.tcseq",
        incremental_config,
    )
    incremental_cursor = PreviewCursor(
        incremental_workspace,
        expected_version_hash=incremental.version_hash,
    )
    incremental_cursor.refresh()
    incremental_cursor.advance_to_end()
    incremental_compare = incremental_cursor.compare_offline(
        load_offline_report((output / "offline" / "incremental_equal.tcseq").with_suffix(".tcseq.report.json"))
    )
    checks.append(
        {
            "check": "incremental_offline_equal",
            "ok": incremental_compare["ok"] and incremental.version_hash == offline_incremental.version_hash,
        }
    )

    low_memory = MemoryConfig(
        parallelism=2,
        tile_budget_bytes=1000,
        run_budget_bytes=2000,
        downgrade_profiles=("full", "banded", "streaming"),
        estimator_overhead_bytes=12_288,
    )
    rejected_config = replace(incremental_config, tone_map_curve="aces", memory=low_memory)
    current_before_reject = VersionStore(incremental_workspace).load_current().version_hash
    reject_code = None
    try:
        convert_incremental(incremental_input, rejected_config, incremental_workspace)
    except TileForgeError as exc:
        reject_code = exc.code
        reject_details = exc.to_dict()
    current_after_reject = VersionStore(incremental_workspace).load_current().version_hash
    checks.append(
        {
            "check": "budget_rejection_preserves_previous",
            "ok": reject_code == E_BUDGET_EXCEEDED and current_before_reject == current_after_reject,
            "code": reject_code,
            "details": reject_details if reject_code else None,
        }
    )

    downgrade_entry = sample_manifest["samples"][2]
    downgrade_input = samples_dir / downgrade_entry["sequence"]
    downgrade_config = JobConfig.load(samples_dir / downgrade_entry["config"])
    downgrade_config = replace(
        downgrade_config,
        memory=MemoryConfig(
            parallelism=2,
            tile_budget_bytes=30_000,
            run_budget_bytes=60_000,
            downgrade_profiles=("full", "banded", "streaming"),
            estimator_overhead_bytes=12_288,
        ),
    )
    downgrade_workspace = output / "workbenches" / "declared_downgrade"
    downgrade = convert_incremental(downgrade_input, downgrade_config, downgrade_workspace)
    checks.append(
        {
            "check": "declared_downgrade_stage",
            "ok": all(stage == "banded" for stage in downgrade.downgrade_stages.values()),
            "stages": {str(key): value for key, value in sorted(downgrade.downgrade_stages.items())},
        }
    )

    recovery_entry = sample_manifest["samples"][1]
    recovery_input = samples_dir / recovery_entry["sequence"]
    recovery_config_path = samples_dir / recovery_entry["config"]
    recovery_workspace = output / "workbenches" / "recovery"
    crash_process = subprocess.run(
        [
            sys.executable,
            "-m",
            "tileforge",
            "convert",
            "--input",
            str(recovery_input),
            "--config",
            str(recovery_config_path),
            "--workspace",
            str(recovery_workspace),
            "--crash-after-commits",
            "5",
        ],
        cwd=ROOT,
        env=os.environ.copy(),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    committed_before_resume = sorted(
        path.name
        for path in (recovery_workspace / "runs").glob("*/commits/*.json")
    )
    newly_committed: list[int] = []
    recovery_summary = convert_incremental(
        recovery_input,
        JobConfig.load(recovery_config_path),
        recovery_workspace,
        on_commit=lambda tile_id, chain: newly_committed.append(tile_id),
    )
    recovery_offline = offline_convert(
        recovery_input,
        output / "offline" / "recovery_equal.tcseq",
        JobConfig.load(recovery_config_path),
    )
    checks.append(
        {
            "check": "forced_termination_recovery",
            "ok": crash_process.returncode == 75
            and len(committed_before_resume) == 5
            and len(newly_committed) == len(VersionStore(recovery_workspace).load_current().sequence.blocks) - 5
            and recovery_summary.version_hash == recovery_offline.version_hash,
            "crash_exit_code": crash_process.returncode,
            "committed_before_resume": committed_before_resume,
            "newly_committed": newly_committed,
            "recovered_version_hash": recovery_summary.version_hash,
            "offline_hash": recovery_offline.version_hash,
        }
    )

    rollback_target = old_current.version_id
    rollback_manifest = VersionStore(incremental_workspace).rollback(
        rollback_target,
        expected_current_hash=incremental.version_hash,
    )
    rolled_frame = preview_frame(incremental_workspace)
    checks.append(
        {
            "check": "atomic_history_rollback",
            "ok": rolled_frame["version_id"] == rollback_target
            and rolled_frame["version_hash"] == old_current.version_hash
            and rollback_manifest["version_hash"] == old_current.version_hash,
            "rolled_version_id": rolled_frame["version_id"],
            "rolled_version_hash": rolled_frame["version_hash"],
        }
    )

    try:
        PreviewCursor(incremental_workspace, expected_version_hash=incremental.version_hash).refresh()
        stale_code = None
    except TileForgeError as exc:
        stale_code = exc.code
    checks.append({"check": "stale_version_rejected", "ok": stale_code == E_VERSION_STALE, "code": stale_code})

    for lock_file in output.rglob("*.lock"):
        lock_file.unlink()
    trace_files = sorted(str(path) for path in output.rglob("*.jsonl"))
    all_events = [event for path in output.rglob("*.jsonl") for event in _trace_events(path)]
    report = {
        "schema": 1,
        "kind": "tileforge-acceptance",
        "ok": all(check["ok"] for check in checks),
        "seed": seed,
        "environment": {
            "python": sys.version.split()[0],
            "numpy_available": _module_available("numpy"),
            "pyside6_available": _module_available("PySide6"),
            "container_used": False,
            "network_used": False,
        },
        "baseline_manifest_hash": sample_manifest["manifest_hash"],
        "samples": sample_results,
        "checks": checks,
        "incremental": {
            "version_id": incremental.version_id,
            "version_hash": incremental.version_hash,
            "affected_tiles": incremental.affected_tiles,
            "reused_tiles": incremental.reused_tiles,
        },
        "recovery": {
            "version_hash": recovery_summary.version_hash,
            "newly_committed_tiles": newly_committed,
        },
        "trace_files": trace_files,
        "trace_event_count": len(all_events),
    }
    _write_json(logs_dir / "acceptance_report.json", report)
    _write_json(output / "report.json", report)
    print(json.dumps({key: report[key] for key in ("ok", "seed", "samples", "checks", "trace_event_count")}, indent=2, ensure_ascii=False))
    return 0 if report["ok"] else 1


def _module_available(name: str) -> bool:
    try:
        __import__(name)
        return True
    except ImportError:
        return False


if __name__ == "__main__":
    raise SystemExit(main())
