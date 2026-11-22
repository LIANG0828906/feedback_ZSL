#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import platform
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from terrain_pipeline import BakeParams, Pipeline
from terrain_pipeline.bake import CancellationToken
from terrain_pipeline.compare import compare_snapshots
from terrain_pipeline.errors import (
    E_BAKE,
    E_CACHE,
    E_CANCEL,
    E_DIFF,
    E_RESOURCE,
    E_SCHEMA,
    PipelineError,
)
from terrain_pipeline.generator import generate_project
from terrain_pipeline.loader import load_project
from terrain_pipeline.pipeline import inspect_cache
from terrain_pipeline.util import atomic_write_json, read_json, sha256_file


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Run the end-to-end terrain pipeline acceptance demo"
    )
    parser.add_argument("--runtime", default="artifacts/runtime/verification")
    parser.add_argument("--report-json", default="reports/verification.json")
    parser.add_argument("--report-md", default="reports/verification.md")
    args = parser.parse_args()
    runtime = (ROOT / args.runtime).resolve()
    if runtime.exists():
        shutil.rmtree(runtime)
    runtime.mkdir(parents=True)

    report: dict[str, Any] = {
        "ok": False,
        "seed": 20261123,
        "runtime": _display_path(runtime),
        "environment": {
            "python": platform.python_version(),
            "platform": platform.platform(),
            "numpy": np.__version__,
        },
        "steps": {},
    }
    try:
        import PyQt5.QtCore

        report["environment"]["pyqt5"] = PyQt5.QtCore.PYQT_VERSION_STR
    except ImportError:
        report["environment"]["pyqt5"] = None
    try:
        import moderngl

        report["environment"]["moderngl"] = moderngl.__version__
    except ImportError:
        report["environment"]["moderngl"] = None
    try:
        project_dir = runtime / "project"
        manifest = generate_project(
            project_dir,
            seed=20261123,
            grid_tiles=(4, 3),
            tile_size=16,
            subdivisions=4,
            points_per_tile=96,
        )
        report["project"] = {
            "tile_count": len(manifest["tiles"]),
            "grid_tiles": manifest["grid_tiles"],
            "tile_size": manifest["tile_size"],
            "subdivisions": manifest["subdivisions"],
            "material_layers": [item["name"] for item in manifest["materials"]],
            "points_per_tile_target": 96,
        }

        params = BakeParams(
            subdivisions=4,
            normal_strength=1.0,
            min_layer_weight=0.12,
            max_vertices_per_thread=262_144,
            max_workers=4,
        )
        baseline_cache = runtime / "cache_baseline"
        baseline_pipeline = Pipeline(project_dir, baseline_cache, params=params)
        baseline = baseline_pipeline.run(mode="full")
        _require(baseline.success, "baseline bake failed", baseline.error)
        baseline_snapshot = read_json(baseline_cache / "snapshot.json")
        truth_ok = 0
        for entry in baseline_snapshot["tiles"]:
            block = read_json(baseline_cache / entry["block_dir"] / "block.json")
            if block["truth_check"]["ok"]:
                truth_ok += 1
        _require(truth_ok == len(baseline_snapshot["tiles"]), "truth checks did not all pass")
        report["steps"]["baseline"] = {
            "success": True,
            "tiles": baseline_snapshot["tile_count"],
            "truth_checks_passed": truth_ok,
            "snapshot_sha256": sha256_file(baseline_cache / "snapshot.json"),
            "affected": len(baseline.invalidations.affected),
            "rebuilt": len(baseline.rebuilt),
        }

        incremental_cache = runtime / "cache_incremental"
        shutil.copytree(baseline_cache, incremental_cache)
        incremental_pipeline = Pipeline(project_dir, incremental_cache, params=params)
        _require(
            incremental_pipeline.snapshot is not None,
            "incremental cache snapshot did not load",
        )
        material_report = incremental_pipeline.apply_material_edit("T0005", "rock", 1.8)
        height_report = incremental_pipeline.apply_height_edit("T0006", 8, 8, 14.25)
        edits_path = runtime / "edits.json"
        incremental_pipeline.save_edits(edits_path)
        incremental = incremental_pipeline.run(mode="incremental")
        _require(incremental.success, "incremental bake failed", incremental.error)
        report["steps"]["incremental"] = {
            "success": True,
            "trigger": incremental.invalidations.trigger,
            "affected": incremental.invalidations.affected,
            "reused": incremental.invalidations.reused,
            "rebuilt": incremental.rebuilt,
            "reused_count": len(incremental.reused),
            "rebuilt_count": len(incremental.rebuilt),
            "total_ms": round(incremental.total_ms, 3),
            "snapshot_sha256": sha256_file(incremental_cache / "snapshot.json"),
            "material_affected": material_report.affected,
            "height_affected": height_report.affected,
        }

        full_serial_cache = runtime / "cache_full_serial"
        serial = Pipeline(
            project_dir,
            full_serial_cache,
            params=BakeParams(subdivisions=4, max_workers=1),
            edits_file=edits_path,
        ).run(mode="full")
        _require(serial.success, "serial full rebuild failed", serial.error)
        full_parallel_cache = runtime / "cache_full_parallel"
        parallel = Pipeline(
            project_dir,
            full_parallel_cache,
            params=BakeParams(subdivisions=4, max_workers=4),
            edits_file=edits_path,
        ).run(mode="full")
        _require(parallel.success, "parallel full rebuild failed", parallel.error)
        parity = compare_snapshots(incremental_cache, full_parallel_cache, raise_on_diff=False)
        serial_parity = compare_snapshots(
            full_serial_cache, full_parallel_cache, raise_on_diff=False
        )
        _require(parity["equal"], "incremental/full byte parity failed", parity)
        _require(serial_parity["equal"], "serial/parallel byte parity failed", serial_parity)
        _require(
            serial.invalidations.to_dict() == parallel.invalidations.to_dict(),
            "serial/parallel invalidation records differ",
        )
        report["steps"]["full_and_concurrency"] = {
            "serial_success": True,
            "parallel_success": True,
            "incremental_equals_full": parity,
            "serial_equals_parallel": serial_parity,
            "invalidation_record_equal": True,
            "snapshot_sha256": sha256_file(full_parallel_cache / "snapshot.json"),
            "serial_effective_workers": serial.effective_workers,
            "parallel_effective_workers": parallel.effective_workers,
        }

        cache_hit = incremental_pipeline.run()
        _require(cache_hit.success, "cache-hit run failed", cache_hit.error)
        _require(not cache_hit.rebuilt, "cache-hit run unexpectedly rebuilt blocks")
        _require(
            set(cache_hit.reused) == set(incremental_pipeline.project.tiles),
            "cache-hit did not reuse all blocks",
        )
        report["steps"]["cache_hit"] = {
            "success": True,
            "reused": cache_hit.reused,
            "rebuilt": cache_hit.rebuilt,
            "trigger": cache_hit.invalidations.trigger,
        }

        recovery_cache = runtime / "cache_recovery"
        child_code = f"""
from terrain_pipeline import Pipeline, BakeParams
pipeline = Pipeline(
    {str(project_dir)!r},
    {str(recovery_cache)!r},
    params=BakeParams(subdivisions=4, max_workers=4),
    edits_file={str(edits_path)!r},
)
result = pipeline.run(mode="full", fault_tiles={{"T0007"}})
if result.success:
    raise SystemExit(1)
import os
os._exit(23)
"""
        child = subprocess.run(
            [sys.executable, "-c", child_code],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        _require(
            child.returncode == 23,
            "forced-termination child did not exit with the expected code",
            {"returncode": child.returncode, "stderr": child.stderr},
        )
        _require(
            not (recovery_cache / "snapshot.json").exists(),
            "failed run published a snapshot",
        )
        pending = read_json(recovery_cache / "pending.json")
        _require(
            "T0007" not in pending["committed"],
            "failed tile polluted the committed ledger",
        )
        _require(len(pending["committed"]) == 11, "unexpected partial commit count")
        resumed_pipeline = Pipeline(
            project_dir,
            recovery_cache,
            params=BakeParams(subdivisions=4, max_workers=4),
            edits_file=edits_path,
        )
        recovered_at_restart = len(resumed_pipeline._startup_recovered)
        _require(recovered_at_restart == 11, "pending recovery did not load 11 blocks")
        resumed = resumed_pipeline.run()
        _require(resumed.success, "recovery run failed", resumed.error)
        recovery_parity = compare_snapshots(
            recovery_cache, full_parallel_cache, raise_on_diff=False
        )
        _require(
            recovery_parity["equal"],
            "recovery/full byte parity failed",
            recovery_parity,
        )
        report["steps"]["recovery"] = {
            "forced_termination_exit_code": child.returncode,
            "snapshot_after_failure": False,
            "committed_blocks": len(pending["committed"]),
            "recovered_at_restart": recovered_at_restart,
            "rebuilt_on_retry": resumed.rebuilt,
            "recovery_equals_full": recovery_parity,
        }

        mismatch_project = runtime / "project_mismatch"
        shutil.copytree(project_dir, mismatch_project)
        mismatch_cache = runtime / "cache_mismatch"
        shutil.copytree(baseline_cache, mismatch_cache)
        mismatch_height_path = mismatch_project / "tiles" / "T0000" / "height.npy"
        mismatch_height = np.load(mismatch_height_path)
        mismatch_height[1, 1] += 0.75
        np.save(mismatch_height_path, mismatch_height)
        mismatch_pipeline = Pipeline(
            mismatch_project,
            mismatch_cache,
            params=params,
        )
        _require(
            mismatch_pipeline.startup_reason == "global_content_hash_mismatch",
            "global mismatch was not detected",
            {"startup_reason": mismatch_pipeline.startup_reason},
        )
        _require(
            mismatch_pipeline.snapshot is None,
            "mismatched snapshot was partially retained",
        )
        mismatch_result = mismatch_pipeline.run()
        _require(
            mismatch_result.success,
            "global-mismatch full rebuild failed",
            mismatch_result.error,
        )
        _require(
            set(mismatch_result.rebuilt) == set(mismatch_pipeline.project.tiles),
            "global mismatch did not force a full rebuild",
        )
        report["steps"]["global_invalidation"] = {
            "startup_reason": "global_content_hash_mismatch",
            "snapshot_retained": False,
            "rebuilt_all": True,
            "rebuilt_count": len(mismatch_result.rebuilt),
        }

        report["steps"]["errors"] = _exercise_errors(
            runtime,
            project_dir,
            baseline_cache,
            full_parallel_cache,
            params,
        )
        report["steps"]["gui_smoke"] = _gui_smoke(
            project_dir,
            baseline_cache,
            params,
            ROOT / "reports" / "gui_smoke.png",
        )
        report["ok"] = True
    except Exception as exc:
        report["failure"] = {
            "type": type(exc).__name__,
            "message": str(exc),
            "details": getattr(exc, "details", None),
        }

    report_json = (ROOT / args.report_json).resolve()
    report_md = (ROOT / args.report_md).resolve()
    atomic_write_json(report_json, report)
    report_md.parent.mkdir(parents=True, exist_ok=True)
    report_md.write_text(_markdown(report), encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if report["ok"] else 1


def _exercise_errors(
    runtime: Path,
    project_dir: Path,
    baseline_cache: Path,
    full_cache: Path,
    params: BakeParams,
) -> dict[str, Any]:
    results: dict[str, Any] = {}

    schema_project = runtime / "project_schema"
    shutil.copytree(project_dir, schema_project)
    manifest = read_json(schema_project / "manifest.json")
    del manifest["tiles"][0]["height_file"]
    atomic_write_json(schema_project / "manifest.json", manifest)
    try:
        load_project(schema_project)
    except PipelineError as exc:
        _require(
            exc.code == E_SCHEMA and exc.tile_id == "T0000",
            "missing field error lacked context",
            exc.to_dict(),
        )
        results["E_SCHEMA_missing_field"] = {
            "code": exc.code,
            "tile_id": exc.tile_id,
            "field": exc.field,
        }

    range_project = runtime / "project_weight_range"
    shutil.copytree(project_dir, range_project)
    weight_path = range_project / "tiles" / "T0002" / "weights.npy"
    weights = np.load(weight_path).astype(np.int16)
    weights[0, 0, 0] = 300
    np.save(weight_path, weights)
    try:
        load_project(range_project)
    except PipelineError as exc:
        _require(
            exc.code == E_SCHEMA and exc.tile_id == "T0002",
            "weight range error lacked context",
            exc.to_dict(),
        )
        results["E_SCHEMA_weight_range"] = {
            "code": exc.code,
            "tile_id": exc.tile_id,
            "field": exc.field,
        }

    decode_project = runtime / "project_decode"
    shutil.copytree(project_dir, decode_project)
    (decode_project / "tiles" / "T0004" / "height.npy").write_bytes(b"broken npy")
    try:
        load_project(decode_project)
    except PipelineError as exc:
        _require(
            exc.code == "E_DECODE" and exc.tile_id == "T0004",
            "decode error lacked context",
            exc.to_dict(),
        )
        results["E_DECODE_corrupt_npy"] = {
            "code": exc.code,
            "tile_id": exc.tile_id,
            "field": exc.field,
        }

    low_limit = Pipeline(
        project_dir,
        runtime / "cache_low_limit",
        params=BakeParams(subdivisions=4, max_vertices_per_thread=128, max_workers=2),
    ).run()
    _require(
        not low_limit.success and low_limit.error and low_limit.error["code"] == E_RESOURCE,
        "resource limit did not fail with E_RESOURCE",
        low_limit.error,
    )
    results["E_RESOURCE_vertex_limit"] = {
        "code": E_RESOURCE,
        "tile_id": low_limit.error.get("tile_id") if low_limit.error else None,
    }

    fault_result = Pipeline(
        project_dir,
        runtime / "cache_fault",
        params=BakeParams(subdivisions=2, max_workers=2),
    ).run(fault_tiles={"T0001"})
    _require(
        not fault_result.success and fault_result.error and fault_result.error["code"] == E_BAKE,
        "fault injection did not return E_BAKE",
        fault_result.error,
    )
    results["E_BAKE_tile_failure"] = {
        "code": E_BAKE,
        "tile_id": fault_result.error.get("tile_id") if fault_result.error else None,
    }

    cancel_token = CancellationToken()
    cancel_token.cancel()
    cancel_result = Pipeline(
        project_dir,
        runtime / "cache_cancel",
        params=BakeParams(subdivisions=2, max_workers=2),
    ).run(cancel_token=cancel_token)
    _require(
        not cancel_result.success
        and cancel_result.error
        and cancel_result.error["code"] == E_CANCEL,
        "cancellation did not return E_CANCEL",
        cancel_result.error,
    )
    results["E_CANCEL"] = {"code": E_CANCEL, "tile_id": None, "field": None}

    try:
        inspect_cache(runtime / "cache_without_snapshot")
    except PipelineError as exc:
        results["E_CACHE_missing_snapshot"] = {
            "code": exc.code,
            "tile_id": exc.tile_id,
            "field": exc.field,
        }
    _require(
        results.get("E_CACHE_missing_snapshot", {}).get("code") == E_CACHE,
        "missing snapshot did not return E_CACHE",
    )

    diff_cache = runtime / "cache_diff"
    shutil.copytree(full_cache, diff_cache)
    snapshot = read_json(diff_cache / "snapshot.json")
    entry = snapshot["tiles"][0]
    artifact = diff_cache / entry["block_dir"] / "positions.bin"
    payload = bytearray(artifact.read_bytes())
    payload[4] ^= 0x01
    artifact.write_bytes(bytes(payload))
    try:
        compare_snapshots(full_cache, diff_cache)
    except PipelineError as exc:
        _require(exc.code == E_DIFF, "compare did not return E_DIFF", exc.to_dict())
        results["E_DIFF_first_mismatch"] = {
            "code": exc.code,
            "tile_id": exc.tile_id,
            "artifact": exc.field,
        }
    return results


def _gui_smoke(
    project_dir: Path,
    cache: Path,
    params: BakeParams,
    screenshot: Path,
) -> dict[str, Any]:
    try:
        from terrain_pipeline.gui import run_gui
    except ImportError as exc:
        return {"status": "skipped", "reason": type(exc).__name__}
    previous = os.environ.get("QT_QPA_PLATFORM")
    os.environ["QT_QPA_PLATFORM"] = "offscreen"
    try:
        screenshot.parent.mkdir(parents=True, exist_ok=True)
        run_gui(
            project_dir,
            cache,
            params=params,
            smoke_test=True,
            screenshot=screenshot,
        )
        return {
            "status": "passed",
            "platform": "offscreen",
            "window_created": True,
            "screenshot": _display_path(screenshot),
        }
    except Exception as exc:
        return {
            "status": "failed",
            "reason": f"{type(exc).__name__}: {exc}",
        }
    finally:
        if previous is None:
            os.environ.pop("QT_QPA_PLATFORM", None)
        else:
            os.environ["QT_QPA_PLATFORM"] = previous


def _require(condition: bool, message: str, details: Any = None) -> None:
    if not condition:
        raise AssertionError(
            json.dumps(
                {"message": message, "details": details},
                ensure_ascii=False,
                default=str,
            )
        )


def _display_path(path: Path) -> str:
    try:
        return path.relative_to(ROOT).as_posix()
    except ValueError:
        return path.as_posix()


def _markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Terrain Pipeline Verification",
        "",
        f"Result: **{'PASS' if report.get('ok') else 'FAIL'}**",
        "",
        "This report is generated by `python scripts/verify_pipeline.py` using the local deterministic sample.",
        "It contains no network access and no external accounts.",
        "",
        "## Environment",
        "",
        f"- Seed: `{report.get('seed')}`",
        f"- Runtime directory: `{report.get('runtime')}`",
    ]
    project = report.get("project", {})
    if project:
        lines.extend(
            [
                f"- Tiles: `{project.get('tile_count')}` ({project.get('grid_tiles')})",
                f"- Tile size: `{project.get('tile_size')}`; subdivisions: `{project.get('subdivisions')}`",
                f"- Layers: `{', '.join(project.get('material_layers', []))}`",
            ]
        )
    steps = report.get("steps", {})
    baseline = steps.get("baseline", {})
    incremental = steps.get("incremental", {})
    full = steps.get("full_and_concurrency", {})
    recovery = steps.get("recovery", {})
    mismatch = steps.get("global_invalidation", {})
    errors = steps.get("errors", {})
    gui = steps.get("gui_smoke", {})
    if baseline:
        lines.extend(
            [
                "",
                "## Full Baseline",
                "",
                f"- Tiles baked: `{baseline.get('tiles')}`",
                f"- Analytic truth checks: `{baseline.get('truth_checks_passed')}` passed",
                f"- Snapshot SHA-256: `{baseline.get('snapshot_sha256')}`",
            ]
        )
    if incremental:
        lines.extend(
            [
                "",
                "## Edit / Incremental",
                "",
                f"- Affected: `{', '.join(incremental.get('affected', []))}`",
                f"- Reused: `{', '.join(incremental.get('reused', []))}`",
                f"- Rebuilt: `{', '.join(incremental.get('rebuilt', []))}`",
                f"- Rebuilt count / reused count: `{incremental.get('rebuilt_count')}` / `{incremental.get('reused_count')}`",
                f"- Incremental wall time: `{incremental.get('total_ms')}` ms",
            ]
        )
    if full:
        lines.extend(
            [
                "",
                "## Byte Parity And Concurrency",
                "",
                f"- Incremental equals full: `{full.get('incremental_equals_full', {}).get('equal')}`",
                f"- Serial equals parallel: `{full.get('serial_equals_parallel', {}).get('equal')}`",
                f"- Serial/parallel invalidation records equal: `{full.get('invalidation_record_equal')}`",
                f"- Effective workers: `{full.get('serial_effective_workers')}` / `{full.get('parallel_effective_workers')}`",
            ]
        )
    if recovery:
        lines.extend(
            [
                "",
                "## Recovery",
                "",
                f"- Forced-termination exit code: `{recovery.get('forced_termination_exit_code')}`",
                f"- Complete snapshot after failure: `{recovery.get('snapshot_after_failure')}`",
                f"- Committed blocks: `{recovery.get('committed_blocks')}`",
                f"- Recovered at restart: `{recovery.get('recovered_at_restart')}`",
                f"- Rebuilt on retry: `{', '.join(recovery.get('rebuilt_on_retry', []))}`",
                f"- Recovery equals full: `{recovery.get('recovery_equals_full', {}).get('equal')}`",
            ]
        )
    if mismatch:
        lines.extend(
            [
                "",
                "## Global Invalidation",
                "",
                f"- Startup reason: `{mismatch.get('startup_reason')}`",
                f"- Snapshot retained: `{mismatch.get('snapshot_retained')}`",
                f"- Rebuilt all tiles: `{mismatch.get('rebuilt_all')}` (`{mismatch.get('rebuilt_count')}`)",
            ]
        )
    if errors:
        lines.extend(
            [
                "",
                "## Error Codes",
                "",
                "| Case | Code | Tile | Field / Artifact |",
                "|---|---|---|---|",
            ]
        )
        for name, value in errors.items():
            lines.append(
                f"| `{name}` | `{value.get('code')}` | `{value.get('tile_id')}` | "
                f"`{value.get('field', value.get('artifact', ''))}` |"
            )
    if gui:
        lines.extend(
            [
                "",
                "## GUI Smoke",
                "",
                f"- Status: `{gui.get('status')}`",
                f"- Qt platform: `{gui.get('platform', '-')}`",
                f"- Window created: `{gui.get('window_created', False)}`",
                f"- Screenshot: `{gui.get('screenshot', '-')}`",
            ]
        )
    if report.get("failure"):
        lines.extend(
            [
                "",
                "## Failure",
                "",
                f"```json\n{json.dumps(report['failure'], ensure_ascii=False, indent=2)}\n```",
            ]
        )
    lines.append("")
    return "\n".join(lines)


if __name__ == "__main__":
    raise SystemExit(main())
