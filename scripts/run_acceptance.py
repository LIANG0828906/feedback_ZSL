from __future__ import annotations

import importlib.util
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from motion_workbench.constraints import load_constraints
from motion_workbench.errors import E_COMMIT_FAILED, WorkbenchError
from motion_workbench.generator import SampleSpec, generate_sample
from motion_workbench.session import WorkbenchSession, resume_session


def _write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    os.replace(temporary, path)


def _compare_frames(left, right, frame_count: int) -> dict:
    mismatches = []
    for frame in range(frame_count):
        a = left.state.frame_solution(frame)
        b = right.state.frame_solution(frame)
        if a is None or b is None:
            mismatches.append(
                {
                    "frame": frame,
                    "left": a is not None,
                    "right": b is not None,
                }
            )
            continue
        if a.frame_hash != b.frame_hash:
            mismatches.append(
                {
                    "frame": frame,
                    "left_hash": a.frame_hash,
                    "right_hash": b.frame_hash,
                }
            )
    return {
        "frame_count": frame_count,
        "all_frame_hashes_equal": not mismatches,
        "mismatches": mismatches,
    }


def main() -> int:
    acceptance = ROOT / "artifacts" / "acceptance"
    if acceptance.exists():
        shutil.rmtree(acceptance)
    samples = acceptance / "samples"
    samples.mkdir(parents=True)
    report: dict = {
        "schema": "motion-workbench/verification/v1",
        "environment": {
            "python": sys.version,
            "platform": platform.platform(),
            "numpy_available": importlib.util.find_spec("numpy") is not None,
            "pyside6_available": importlib.util.find_spec("PySide6") is not None,
        },
        "checks": {},
    }

    tests = subprocess.run(
        [sys.executable, "-m", "unittest", "discover", "-v"],
        cwd=str(ROOT),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=180,
    )
    report["checks"]["automated_tests"] = {
        "command": "python -m unittest discover -v",
        "returncode": tests.returncode,
        "passed": tests.returncode == 0,
        "tail": tests.stdout.splitlines()[-6:],
    }

    spec = SampleSpec(frames=24, joints=8, conflict="none", seed=77)
    generated = generate_sample(
        samples, spec, keep_baseline_session=False
    )
    bvh_path = generated["bvh_path"]
    constraints_path = generated["constraints_path"]

    full_session = WorkbenchSession(
        bvh_path, constraints_path, acceptance / "full-session"
    )
    full_report = full_session.solve(run_id="acceptance-full", chunk_size=6)
    full_state = full_session.store.load_committed(
        full_session.bvh, constraints=full_session.constraints
    )

    document = json.loads(constraints_path.read_text(encoding="utf-8"))
    for constraint in document["constraints"]:
        if constraint["id"] == "ee:effector":
            target = constraint["targets"][10][1]
            constraint["targets"][10][1] = [
                target[0] + 0.006,
                target[1],
                target[2],
            ]
    edited_path = acceptance / "edited-constraints.json"
    _write_json(edited_path, document)
    incremental_session = WorkbenchSession(
        bvh_path, constraints_path, acceptance / "incremental-session"
    )
    incremental_session.solve(run_id="acceptance-baseline", chunk_size=6)
    incremental_session.set_constraints(
        load_constraints(edited_path, incremental_session.bvh)
    )
    incremental_report = incremental_session.solve(
        run_id="acceptance-incremental", chunk_size=6
    )
    fresh_session = WorkbenchSession(
        bvh_path, edited_path, acceptance / "fresh-session"
    )
    fresh_report = fresh_session.solve(run_id="acceptance-fresh")
    report["checks"]["incremental"] = {
        "impact_ranges": [list(pair) for pair in incremental_report.plan.frames],
        "computed_frames": list(incremental_report.computed_frames),
        "cached_frame_count": len(incremental_report.cached_frames),
        "reasons": list(incremental_report.plan.reasons),
        "conservative": incremental_report.plan.conservative,
        "comparison": _compare_frames(
            incremental_report, fresh_report, spec.frames
        ),
        "incremental_residual_digest": fresh_report.to_dict()["residual_digest"],
    }

    crash_dir = acceptance / "crash-session"
    crash_command = [
        sys.executable,
        "-m",
        "motion_workbench.cli",
        "solve",
        "--bvh",
        str(bvh_path),
        "--constraints",
        str(constraints_path),
        "--session",
        str(crash_dir),
        "--chunk-size",
        "6",
        "--crash-after-commits",
        "2",
    ]
    crashed = subprocess.run(
        crash_command,
        cwd=str(ROOT),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=120,
    )
    resumed = resume_session(crash_dir)
    before_resume = resumed.store.load_committed(
        resumed.bvh, constraints=resumed.constraints
    )
    resume_report = resumed.solve(
        run_id="acceptance-resume", chunk_size=6
    )
    resumed_state = resumed.store.load_committed(
        resumed.bvh, constraints=resumed.constraints
    )
    full_original = WorkbenchSession(
        bvh_path, constraints_path, acceptance / "full-original-copy"
    ).solve(run_id="acceptance-original-copy")
    report["checks"]["hard_kill_resume"] = {
        "exit_code": crashed.returncode,
        "expected_exit_code": 137,
        "pre_resume_committed_ranges": [
            list(pair) for pair in before_resume.committed_ranges
        ],
        "pre_resume_next_frame": before_resume.next_frame,
        "resume_computed_frames": list(resume_report.computed_frames),
        "resume_cached_frames": list(resume_report.cached_frames),
        "post_resume_next_frame": resumed_state.next_frame,
        "post_resume_chain_head": resumed_state.chain_head,
        "comparison_to_uninterrupted": _compare_frames(
            resume_report, full_original, spec.frames
        ),
    }

    conflict_generated = generate_sample(
        samples,
        SampleSpec(
            frames=10, joints=6, conflict="ee_unreachable", seed=78
        ),
        keep_baseline_session=False,
    )
    conflict_session = WorkbenchSession(
        conflict_generated["bvh_path"],
        conflict_generated["constraints_path"],
        acceptance / "conflict-session",
    )
    conflict_report = conflict_session.solve(run_id="acceptance-conflict")
    report["checks"]["conflict_diagnostics"] = {
        "status": conflict_report.status,
        "first_conflict_frame": (
            None
            if conflict_report.conflict is None
            else conflict_report.conflict["diagnostic"]["frame"]
        ),
        "committed_ranges": [
            list(pair) for pair in conflict_report.state.committed_ranges
        ],
        "barrier_frame": (
            None
            if conflict_report.conflict is None
            else conflict_report.conflict["report"]["barrier_frame"]
        ),
        "report_file": (
            None
            if conflict_report.conflict is None
            else conflict_report.conflict["report"]["report_file"]
        ),
    }

    fault_dir = acceptance / "fault-session"

    def fail_manifest(stage, path, payload):
        if stage == "manifest" and payload and payload.get("commits"):
            raise RuntimeError("acceptance injected commit failure")

    fault_session = WorkbenchSession(
        bvh_path,
        constraints_path,
        fault_dir,
        fault_hook=fail_manifest,
    )
    fault_code = None
    try:
        fault_session.solve(run_id="acceptance-fault", chunk_size=6)
    except WorkbenchError as exc:
        fault_code = exc.code
    fault_state = fault_session.store.load_committed(
        fault_session.bvh, constraints=fault_session.constraints
    )
    report["checks"]["atomic_commit_failure"] = {
        "error_code": fault_code,
        "expected_error_code": E_COMMIT_FAILED,
        "committed_frame_count_after_failure": len(fault_state.frame_map),
        "chain_head_after_failure": fault_state.chain_head,
    }

    trace_lines = resumed.store.read_trace()
    report["checks"]["trace"] = {
        "line_count": len(trace_lines),
        "has_frame_events": any(
            event.get("event") == "frame" for event in trace_lines
        ),
        "frame_event_fields": (
            sorted(
                next(
                    event
                    for event in trace_lines
                    if event.get("event") == "frame"
                ).keys()
            )
            if any(event.get("event") == "frame" for event in trace_lines)
            else []
        ),
    }
    report["checks"]["full_solve"] = {
        "status": full_report.status,
        "committed_ranges": [
            list(pair) for pair in full_state.committed_ranges
        ],
        "chain_head": full_state.chain_head,
        "residual_digest": full_report.to_dict()["residual_digest"],
        "baseline_residual_digest": generated["baseline"]["residual_digest"],
    }
    report["ok"] = all(
        [
            report["checks"]["automated_tests"]["passed"],
            report["checks"]["incremental"]["comparison"][
                "all_frame_hashes_equal"
            ],
            report["checks"]["hard_kill_resume"]["exit_code"] == 137,
            report["checks"]["hard_kill_resume"]["comparison_to_uninterrupted"][
                "all_frame_hashes_equal"
            ],
            report["checks"]["conflict_diagnostics"]["status"] == "conflict",
            report["checks"]["atomic_commit_failure"]["error_code"]
            == E_COMMIT_FAILED,
            report["checks"]["atomic_commit_failure"][
                "committed_frame_count_after_failure"
            ]
            == 0,
            report["checks"]["trace"]["has_frame_events"],
        ]
    )
    output = ROOT / "artifacts" / "verification.json"
    _write_json(output, report)
    print(json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
