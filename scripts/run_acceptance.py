"""Run the complete local acceptance flow and retain logs under artifacts/acceptance."""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from probe_baker.acceptance import run_incremental_demo
from probe_baker.bake import BakeEngine
from probe_baker.canonical import TraceLog, write_json_atomic
from probe_baker.comparison import compare_strategies
from probe_baker.samples import write_sample_scenes
from probe_baker.scene import load_scene
from probe_baker.strategy import STRATEGY_DENSE, BakeConfig


def _run(command: list[str], *, expected_returncode: int = 0) -> subprocess.CompletedProcess[str]:
    completed = subprocess.run(
        command,
        cwd=ROOT,
        text=True,
        capture_output=True,
        check=False,
    )
    if completed.returncode != expected_returncode:
        raise RuntimeError(
            f"command failed ({completed.returncode}, expected {expected_returncode}): "
            f"{' '.join(command)}\nstdout:\n{completed.stdout}\nstderr:\n{completed.stderr}"
        )
    return completed


def _json_output(completed: subprocess.CompletedProcess[str]) -> dict[str, Any]:
    return json.loads(completed.stdout)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--clean", action="store_true")
    parser.add_argument("--skip-tests", action="store_true")
    args = parser.parse_args()
    output = ROOT / "artifacts" / "acceptance"
    if output.exists() and args.clean:
        shutil.rmtree(output)
    output.mkdir(parents=True, exist_ok=True)
    trace = TraceLog(output / "trace.jsonl")
    started = time.perf_counter()

    test_result = None
    if not args.skip_tests:
        tests = _run([sys.executable, "-m", "pytest", "-q"])
        test_result = {
            "returncode": tests.returncode,
            "stdout": tests.stdout.strip(),
        }
        trace.event("acceptance_tests_passed", **test_result)

    samples_dir = output / "samples"
    room_path, city_path = write_sample_scenes(samples_dir)
    room = load_scene(room_path)
    city = load_scene(city_path)
    trace.event("acceptance_samples_generated", room=str(room_path), city=str(city_path))

    base_config = BakeConfig(
        spacing=1.0,
        sh_order=2,
        direction_count=32,
        compensation_strength=0.3,
        strategy=STRATEGY_DENSE,
        chunk_size=16,
    )
    incremental = run_incremental_demo(room, output / "incremental", base_config)
    trace.event("acceptance_incremental_verified", summary=incremental)

    comparison_config = BakeConfig(
        spacing=1.5,
        sh_order=3,
        direction_count=64,
        compensation_strength=0.35,
        strategy=STRATEGY_DENSE,
        chunk_size=64,
    )
    comparison = compare_strategies(
        room,
        comparison_config,
        engine=BakeEngine(trace=TraceLog(output / "comparison" / "trace.jsonl")),
    )
    write_json_atomic(output / "comparison" / "comparison.json", comparison)
    trace.event("acceptance_strategies_compared", summary=comparison)

    viewer = _run(
        [
            sys.executable,
            "-m",
            "probe_baker",
            "viewer",
            "--scene",
            str(room_path),
            "--spacing",
            "2",
            "--order",
            "2",
            "--directions",
            "24",
            "--strategy",
            "dense_per_probe",
            "--headless",
            "--out",
            str(output / "viewer"),
            "--log",
            str(output / "viewer" / "trace.jsonl"),
        ]
    )
    trace.event("acceptance_headless_viewer", stdout=viewer.stdout.strip())

    forced_root = output / "forced" / "snapshots"
    forced_trace = output / "forced" / "trace.jsonl"
    killed = _run(
        [
            sys.executable,
            "-m",
            "probe_baker",
            "snapshot-kill",
            "--scene",
            str(city_path),
            "--spacing",
            "2",
            "--order",
            "2",
            "--directions",
            "24",
            "--strategy",
            "dense_per_probe",
            "--chunk-size",
            "64",
            "--snapshot-root",
            str(forced_root),
            "--log",
            str(forced_trace),
            "--version",
            "1",
            "--after-chunks",
            "2",
        ],
        expected_returncode=23,
    )
    manifest_exists = (forced_root / "v00000001" / "manifest.json").exists()
    if manifest_exists:
        raise RuntimeError("forced termination unexpectedly committed a manifest")
    original_hash = None
    for line in forced_trace.read_text(encoding="utf-8").splitlines():
        event = json.loads(line)
        if event.get("event") == "bake_completed":
            original_hash = event["content_hash"]
    if original_hash is None:
        raise RuntimeError("forced termination trace has no completed bake hash")
    trace.event(
        "acceptance_forced_termination",
        process_exit_code=killed.returncode,
        manifest_exists=manifest_exists,
        original_content_hash=original_hash,
        clean_exit=False,
    )

    recovery = _json_output(
        _run(
            [
                sys.executable,
                "-m",
                "probe_baker",
                "recover",
                "--scene",
                str(city_path),
                "--snapshot-root",
                str(forced_root),
                "--snapshot-dir",
                str(forced_root / "v00000001"),
                "--spacing",
                "2",
                "--order",
                "2",
                "--directions",
                "24",
                "--strategy",
                "dense_per_probe",
                "--chunk-size",
                "64",
                "--out",
                str(output / "recovery"),
                "--log",
                str(forced_trace),
            ]
        )
    )
    if recovery["clean_exit"] is not False or recovery["published"] is not False:
        raise RuntimeError("forced termination was incorrectly reported as a clean publication")
    if recovery["resampled_count"] != recovery["pending_count"]:
        raise RuntimeError("recovery did not sample exactly the uncommitted probes")
    recovered_query = _json_output(
        _run(
            [
                sys.executable,
                "-m",
                "probe_baker",
                "query",
                "--snapshot-root",
                str(output / "recovery" / "recovered_snapshots"),
                "--snapshot-dir",
                str(output / "recovery" / "recovered_snapshots" / "v00000001"),
                "--version",
                "1",
                "--expected-hash",
                original_hash,
                "--position",
                "-11.5",
                "0.5",
                "-9.5",
            ]
        )
    )
    if recovered_query["content_hash"] != original_hash:
        raise RuntimeError("recovered snapshot content hash differs from the interrupted state")
    trace.event(
        "acceptance_recovery_verified",
        committed_count=recovery["committed_count"],
        pending_count=recovery["pending_count"],
        resampled_count=recovery["resampled_count"],
        recovered_version=recovered_query["version"],
        recovered_content_hash=recovered_query["content_hash"],
    )

    summary = {
        "tests": test_result,
        "incremental": {
            "versions": [
                {
                    "version": item["version"],
                    "kind": item["kind"],
                    "recomputed_count": item.get("invalidation", {}).get("recomputed_count"),
                    "reasons": item.get("invalidation", {}).get("reasons"),
                }
                for item in incremental["versions"]
            ],
            "history_versions": incremental["history_versions"],
            "stale_query_code": incremental["stale_query"]["code"],
            "old_snapshot_readable_version": incremental[
                "old_snapshot_object_still_readable"
            ]["version"],
        },
        "strategy_comparison": {
            "probe_count": comparison["probe_count"],
            "direction_count": comparison["direction_count"],
            "shared_direction_hash": comparison["shared_direction_hash"],
            "dense_mean_relative_error": comparison["dense"]["mean_relative_error"],
            "sparse_mean_relative_error": comparison["sparse"]["mean_relative_error"],
            "dense_sampled_probes": comparison["dense"]["sampled_probes"],
            "sparse_sampled_probes": comparison["sparse"]["sampled_probes"],
            "direction_coverage_reduced": comparison["direction_coverage_reduced"],
        },
        "headless_viewer": viewer.stdout.strip(),
        "forced_termination_recovery": {
            "kill_exit_code": killed.returncode,
            "manifest_after_kill": manifest_exists,
            "recovery_status": recovery["status"],
            "clean_exit": recovery["clean_exit"],
            "published": recovery["published"],
            "committed_count": recovery["committed_count"],
            "pending_count": recovery["pending_count"],
            "resampled_count": recovery["resampled_count"],
            "recovered_version": recovered_query["version"],
            "recovered_content_hash": recovered_query["content_hash"],
            "matches_pre_kill_hash": recovered_query["content_hash"] == original_hash,
        },
        "trace_logs": {
            "acceptance": str(output / "trace.jsonl"),
            "incremental": str(output / "incremental" / "trace.jsonl"),
            "comparison": str(output / "comparison" / "trace.jsonl"),
            "forced_and_recovery": str(forced_trace),
            "viewer": str(output / "viewer" / "trace.jsonl"),
        },
        "elapsed_seconds": time.perf_counter() - started,
    }
    write_json_atomic(output / "summary.json", summary)
    trace.event("acceptance_completed", summary_path=str(output / "summary.json"))
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

