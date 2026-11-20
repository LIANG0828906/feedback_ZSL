#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import threading
import time
import traceback
from pathlib import Path
from typing import Any

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mesh_repair.binary import file_sha256, load_mesh
from mesh_repair.commit import SessionLock
from mesh_repair.coordinator import RepairConfig, RepairService
from mesh_repair.errors import BusinessError
from mesh_repair.render import RenderLoop
from mesh_repair.samples import generate_sample_bundle
from mesh_repair.snapshot import SnapshotStore


ARTIFACTS = ROOT / "artifacts" / "acceptance"
TRACES = ARTIFACTS / "traces"
CHECKS: list[dict[str, Any]] = []
TRACE_FILES: dict[str, Path] = {}


def check(name: str, condition: bool, detail: Any = None) -> None:
    CHECKS.append({"name": name, "passed": bool(condition), "detail": detail})
    print(("PASS " if condition else "FAIL ") + name)


def standard_config(**overrides: Any) -> RepairConfig:
    values = dict(
        block_shape=(2, 2, 1),
        parallelism=2,
        global_budget_bytes=8 * 1024 * 1024,
        task_budget_bytes=4 * 1024 * 1024,
    )
    values.update(overrides)
    return RepairConfig(**values)


def cli_command(source: Path, output: Path, work: Path) -> list[str]:
    return [
        sys.executable,
        "-m",
        "mesh_repair.cli",
        "repair",
        str(source),
        "-o",
        str(output),
        "--work-dir",
        str(work),
        "--blocks",
        "2,2,1",
        "--parallelism",
        "2",
        "--global-budget",
        "8M",
        "--task-budget",
        "4M",
    ]


def latest_trace(work: Path) -> Path | None:
    traces = sorted((work / "traces").glob("trace-*.jsonl"))
    return traces[-1] if traces else None


def main() -> int:
    if ARTIFACTS.exists():
        shutil.rmtree(ARTIFACTS)
    TRACES.mkdir(parents=True)
    samples_dir = ARTIFACTS / "samples"
    baseline = generate_sample_bundle(samples_dir)
    source = Path(baseline["path"])
    trace = baseline["weld"]
    check(
        "deterministic sample contains hole/nonmanifold/duplicates/fragment",
        baseline["raw_topology"]["boundary_edge_count"] > 0
        and baseline["raw_topology"]["nonmanifold_edge_count"] == 1
        and baseline["weld"]["duplicate_count"] == 6
        and baseline["raw_topology"]["face_component_count"] >= 3,
        {
            "boundary_edges": baseline["raw_topology"]["boundary_edge_count"],
            "nonmanifold_edges": baseline["raw_topology"]["nonmanifold_edge_count"],
            "welded_duplicates": trace["duplicate_count"],
            "components": baseline["raw_topology"]["face_component_count"],
        },
    )

    pytest = subprocess.run(
        [sys.executable, "-m", "pytest", "-q"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=120,
    )
    check(
        "automated unit and integration suite",
        pytest.returncode == 0,
        {"returncode": pytest.returncode, "output": pytest.stdout.strip()},
    )

    normal_output = ARTIFACTS / "repaired.msh"
    normal_work = ARTIFACTS / "work-normal"
    store = SnapshotStore()
    service = RepairService(normal_work)
    render_stop = threading.Event()
    render_loop = RenderLoop(
        store,
        stop_event=render_stop,
        preview_path=ARTIFACTS / "preview.png",
        width=800,
        height=560,
    )
    render_thread = threading.Thread(target=render_loop.run, name="acceptance-render")
    render_thread.start()
    background = service.start_background(
        source,
        normal_output,
        config=standard_config(),
        snapshot_store=store,
    )
    normal_report = background.join()
    render_stop.set()
    render_thread.join(timeout=10)
    render_status = render_loop.status.to_dict() if render_loop.status else None
    TRACE_FILES["normal"] = Path(normal_report["trace_path"])
    check(
        "OpenGL or static renderer is available",
        render_status is not None
        and render_status["mode"] in {"opengl-offscreen", "static-fallback"}
        and Path(render_status["preview_path"]).exists(),
        render_status,
    )
    check(
        "background repair produced a watertight, manifold artifact",
        normal_report["boundary_consistency"]["watertight"]
        and normal_report["evaluation"]["topology"]["nonmanifold_edge_count"] == 0
        and normal_report["evaluation"]["topology"]["boundary_edge_count"] == 0,
        {
            "topology": normal_report["evaluation"]["topology"],
            "boundary_consistency": normal_report["boundary_consistency"],
        },
    )
    check(
        "cross-block boundaries stayed consistent",
        normal_report["boundary_consistency"]["shared_edge_count"] > 0
        and normal_report["boundary_consistency"]["shared_vertex_conflicts"] == 0
        and normal_report["boundary_consistency"]["assembled_from_blocks"] == 4
        and len(normal_report["cross_block_weld_merges"]) > 0
        and all(
            len({endpoint["block_index"] for endpoint in item["endpoints"]}) > 1
            for item in normal_report["cross_block_weld_merges"]
        ),
        {
            "boundary_consistency": normal_report["boundary_consistency"],
            "cross_block_merge_groups": len(normal_report["cross_block_weld_merges"]),
        },
    )
    check(
        "cross-block hole fill waited for four weld-map commits",
        normal_report["hole_fill"]["dependency_block_ids"] == [0, 1, 2, 3]
        and normal_report["hole_fill"]["filled_face_count"] == 4,
        normal_report["hole_fill"],
    )
    check(
        "normal deviation remained inside tolerance",
        normal_report["evaluation"]["normal_deviation"]["max_deg"]
        <= normal_report["evaluation"]["normal_deviation"]["tolerance_deg"],
        normal_report["evaluation"]["normal_deviation"],
    )

    wait_config = standard_config(
        block_shape=(4, 2, 1), global_budget_bytes=28_000
    )
    wait_report = RepairService(ARTIFACTS / "work-wait").repair_file(
        source, ARTIFACTS / "wait-budget.msh", config=wait_config
    )
    wait_trace_path = Path(wait_report["trace_path"])
    wait_trace_text = wait_trace_path.read_text(encoding="utf-8")
    TRACE_FILES["budget_wait"] = wait_trace_path
    check(
        "global budget queues tasks and wakes them on release",
        '"event":"budget_wait"' in wait_trace_text
        and '"event":"budget_grant"' in wait_trace_text
        and wait_report["memory"]["max_active_blocks_stage_a"] == 1,
        wait_report["memory"],
    )

    reject_output = ARTIFACTS / "budget-rejected.msh"
    reject_config = standard_config(
        global_budget_bytes=100_000, task_budget_bytes=20_000
    )
    try:
        RepairService(ARTIFACTS / "work-reject").repair_file(
            source, reject_output, config=reject_config
        )
        reject_error = None
    except BusinessError as exc:
        reject_error = exc.to_dict()
    check(
        "single-task budget rejection localizes block and writes no mesh",
        reject_error is not None
        and reject_error["code"] == "E_BUDGET_EXCEEDED"
        and reject_error["location"]["block_id"] == "block-0000"
        and not reject_output.exists(),
        reject_error,
    )
    reject_trace = latest_trace(ARTIFACTS / "work-reject")
    if reject_trace:
        TRACE_FILES["budget_reject"] = reject_trace

    tolerance_result = RepairService(ARTIFACTS / "work-tolerance").compare_tolerance(
        baseline["variants"]["tolerance_asset"]["path"],
        old_tolerance=1e-6,
        new_tolerance=1e-3,
    )
    check(
        "tolerance change re-welds and reports boundary-loop difference",
        tolerance_result["old_loop_count"] > tolerance_result["new_loop_count"]
        and tolerance_result["old_vertex_count"] > tolerance_result["new_vertex_count"],
        {
            "old_loop_count": tolerance_result["old_loop_count"],
            "new_loop_count": tolerance_result["new_loop_count"],
            "old_vertex_count": tolerance_result["old_vertex_count"],
            "new_vertex_count": tolerance_result["new_vertex_count"],
        },
    )

    injection_results = run_input_injections(source)
    check(
        "index/degenerate/nonfinite/count injections return independent E_MESH_INVALID subcodes",
        all(item["passed"] for item in injection_results),
        injection_results,
    )

    source_sha = file_sha256(source)
    busy_work = ARTIFACTS / "work-busy"
    with SessionLock(busy_work / "locks" / f"{source_sha}.lock"):
        try:
            RepairService(busy_work).repair_file(
                source, ARTIFACTS / "busy.msh", config=standard_config()
            )
            busy_error = None
        except BusinessError as exc:
            busy_error = exc.to_dict()
    check(
        "duplicate start on same mesh returns E_BUSY",
        busy_error is not None
        and busy_error["code"] == "E_BUSY"
        and not (ARTIFACTS / "busy.msh").exists(),
        busy_error,
    )

    crash_output = ARTIFACTS / "recovered.msh"
    crash_work = ARTIFACTS / "work-crash"
    env = dict(os.environ)
    env["MESH_REPAIR_CRASH_AFTER_COMMITS"] = "1"
    crashed = subprocess.run(
        cli_command(source, crash_output, crash_work),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    output_exists_after_crash = crash_output.exists()
    committed_after_crash = len(
        list((crash_work / "sessions").glob("*/blocks/block-*.json"))
    )
    env.pop("MESH_REPAIR_CRASH_AFTER_COMMITS")
    resumed = subprocess.run(
        cli_command(source, crash_output, crash_work),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    recovered_report = (
        json.loads(Path(str(crash_output) + ".report.json").read_text())
        if crash_output.exists()
        else None
    )
    recovered_trace = latest_trace(crash_work)
    if recovered_trace:
        TRACE_FILES["recovery"] = recovered_trace
    check(
        "forced termination leaves only committed blocks and no artifact",
        crashed.returncode == 137
        and committed_after_crash == 1
        and not output_exists_after_crash,
        {
            "returncode": crashed.returncode,
            "committed_blocks": committed_after_crash,
            "output_exists_after_crash": output_exists_after_crash,
        },
    )
    check(
        "resume reuses disk commit and matches uninterrupted output hash",
        resumed.returncode == 0
        and recovered_report is not None
        and file_sha256(crash_output) == file_sha256(normal_output)
        and sum(1 for item in recovered_report["commits"] if item["reused"]) == 1,
        {
            "returncode": resumed.returncode,
            "resumed_sha256": file_sha256(crash_output) if crash_output.exists() else None,
            "uninterrupted_sha256": file_sha256(normal_output),
            "reused_blocks": (
                sum(1 for item in recovered_report["commits"] if item["reused"])
                if recovered_report
                else None
            ),
        },
    )

    corrupt_output = ARTIFACTS / "corrupt-recovery.msh"
    corrupt_work = ARTIFACTS / "work-corrupt"
    env["MESH_REPAIR_CRASH_AFTER_COMMITS"] = "1"
    subprocess.run(
        cli_command(source, corrupt_output, corrupt_work),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    env.pop("MESH_REPAIR_CRASH_AFTER_COMMITS")
    committed_file = next((corrupt_work / "sessions").glob("*/blocks/block-*.json"))
    committed_file.write_bytes(committed_file.read_bytes() + b"\nCORRUPTED")
    corrupt_resume = subprocess.run(
        cli_command(source, corrupt_output, corrupt_work),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
    )
    corrupt_trace = latest_trace(corrupt_work)
    if corrupt_trace:
        TRACE_FILES["corrupt_recovery"] = corrupt_trace
    check(
        "damaged committed snapshot aborts with E_RECOVERY_INVALID and no mesh",
        corrupt_resume.returncode == 6
        and "E_RECOVERY_INVALID" in corrupt_resume.stderr
        and not corrupt_output.exists(),
        {
            "returncode": corrupt_resume.returncode,
            "stderr": corrupt_resume.stderr.strip(),
        },
    )

    cancel_output = ARTIFACTS / "cancelled.msh"
    cancel_work = ARTIFACTS / "work-cancel"
    env["MESH_REPAIR_TEST_BLOCK_DELAY_MS"] = "120"
    cancelled_background = RepairService(cancel_work).start_background(
        source,
        cancel_output,
        config=standard_config(
            block_shape=(2, 2, 1), global_budget_bytes=26_000
        ),
    )
    deadline = time.monotonic() + 8.0
    while time.monotonic() < deadline:
        if list((cancel_work / "sessions").glob("*/blocks/block-*.json")):
            break
        time.sleep(0.01)
    cancelled_at = "task_boundary"
    cancelled_background.cancel()
    try:
        cancelled_background.join(timeout=15)
        cancel_error = None
    except BusinessError as exc:
        cancel_error = exc.to_dict()
    finally:
        env.pop("MESH_REPAIR_TEST_BLOCK_DELAY_MS", None)
    retained_blocks = len(
        list((cancel_work / "sessions").glob("*/blocks/block-*.json"))
    )
    cancel_trace = latest_trace(cancel_work)
    if cancel_trace:
        TRACE_FILES["cancel"] = cancel_trace
    check(
        "cooperative cancellation keeps committed blocks, releases files and writes no mesh",
        cancel_error is not None
        and cancel_error["code"] == "E_CANCELLED"
        and not cancel_output.exists()
        and retained_blocks >= 1,
        {
            "cancel_error": cancel_error,
            "retained_committed_blocks": retained_blocks,
            "cancel_point": cancelled_at,
        },
    )

    cli_inspect = subprocess.run(
        [sys.executable, "-m", "mesh_repair.cli", "inspect", str(source)],
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=30,
    )
    check(
        "CLI remains available for headless operation",
        cli_inspect.returncode == 0 and '"topology"' in cli_inspect.stdout,
        {"returncode": cli_inspect.returncode},
    )

    for name, path in TRACE_FILES.items():
        shutil.copy2(path, TRACES / f"{name}.jsonl")
    shutil.copy2(samples_dir / "scan_asset_baseline.json", ARTIFACTS / "baseline.json")
    if normal_report.get("output_sha256"):
        shutil.copy2(normal_output, ARTIFACTS / "artifact.msh")

    total = len(CHECKS)
    passed = sum(1 for item in CHECKS if item["passed"])
    report = {
        "ok": passed == total,
        "passed": passed,
        "total": total,
        "checks": CHECKS,
        "normal_run": normal_report,
        "budget_wait_run": wait_report,
        "recovery_run": recovered_report,
        "render": render_status,
        "trace_files": {
            name: str(TRACES / f"{name}.jsonl") for name in TRACE_FILES
        },
        "pytest": {
            "returncode": pytest.returncode,
            "stdout": pytest.stdout.strip(),
            "stderr": pytest.stderr.strip(),
        },
    }
    (ARTIFACTS / "acceptance_report.json").write_text(
        json.dumps(report, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    print(f"acceptance: {passed}/{total} checks passed")
    print(f"report: {ARTIFACTS / 'acceptance_report.json'}")
    return 0 if report["ok"] else 1


def run_input_injections(source: Path) -> list[dict[str, Any]]:
    base = bytearray(source.read_bytes())
    vertex_count = int.from_bytes(base[12:20], "little")
    face_offset = 28 + vertex_count * 3 * 8
    cases: list[tuple[str, str, bytearray]] = []

    bad_index = bytearray(base)
    bad_index[face_offset : face_offset + 4] = (9999).to_bytes(4, "little")
    cases.append(("index_out_of_range", "E_INDEX_OUT_OF_RANGE", bad_index))

    degenerate = bytearray(base)
    first = int.from_bytes(base[face_offset : face_offset + 4], "little")
    degenerate[face_offset + 4 : face_offset + 8] = first.to_bytes(4, "little")
    cases.append(("degenerate_face", "E_DEGENERATE_FACE", degenerate))

    nonfinite = bytearray(base)
    nonfinite[28:36] = np.asarray([np.nan], dtype="<f8").tobytes()
    cases.append(("nonfinite_coordinate", "E_NONFINITE_COORD", nonfinite))

    count_mismatch = bytearray(base + b"\0\0\0\0")
    cases.append(("face_count_mismatch", "E_FACE_COUNT_MISMATCH", count_mismatch))

    results: list[dict[str, Any]] = []
    for index, (name, expected_subcode, payload) in enumerate(cases):
        path = ARTIFACTS / "injections" / f"{name}.msh"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(payload)
        output = ARTIFACTS / "injections" / f"{name}-out.msh"
        try:
            RepairService(ARTIFACTS / "work-injections" / str(index)).repair_file(
                path, output, config=standard_config()
            )
            error = None
        except BusinessError as exc:
            error = exc.to_dict()
        passed = (
            error is not None
            and error["code"] == "E_MESH_INVALID"
            and error.get("subcode") == expected_subcode
            and not output.exists()
        )
        results.append(
            {
                "name": name,
                "expected_subcode": expected_subcode,
                "error": error,
                "output_exists": output.exists(),
                "passed": passed,
            }
        )
        trace_path = latest_trace(ARTIFACTS / "work-injections" / str(index))
        if trace_path:
            TRACE_FILES[f"injection_{name}"] = trace_path
    return results


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except SystemExit:
        raise
    except Exception:
        traceback.print_exc()
        raise SystemExit(2)
