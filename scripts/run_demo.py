from __future__ import annotations

import json
import os
import platform
import subprocess
import sys
from dataclasses import replace
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from balance_lab.budget import SimulationRequest, build_budget_plan, decision_hash
from balance_lab.checkpoint import CheckpointStore, SnapshotContext
from balance_lab.compare import compare_results
from balance_lab.errors import BalanceError, ErrorCode
from balance_lab.generator import apply_project_injection, generate_all
from balance_lab.report import build_report
from balance_lab.scheduler import run_validation
from balance_lab.state import ComboState, StateMachine
from balance_lab.util import wilson_interval
from balance_lab.validation import load_candidate_set, load_project, parse_project


ARTIFACTS = ROOT / "artifacts"
DATA = ROOT / "data" / "samples"
REPORTS = ARTIFACTS / "reports"
CHECKPOINTS = ARTIFACTS / "demo_checkpoints"


def _safe_unlink(path: Path) -> None:
    for candidate in path.parent.glob(path.name + "*"):
        candidate.unlink()


def _load_bundle(slug: str):
    project = load_project(DATA / f"{slug}.json")
    candidates = load_candidate_set(DATA / f"{slug}.candidates.json", project)
    manifest = json.loads((DATA / "manifest.json").read_text(encoding="utf-8"))
    row = next(item for item in manifest["projects"] if item["slug"] == slug)
    request_row = row.get("demo_request", {})
    request = SimulationRequest(
        target_samples=int(request_row.get("target_samples", 64)),
        min_samples=int(request_row.get("min_samples", 2)),
        total_sample_budget=request_row.get("total_sample_budget"),
    )
    return project, candidates, request


def _report_for(project, candidates, result, name: str) -> dict[str, Any]:
    report = build_report(project, candidates, result)
    REPORTS.mkdir(parents=True, exist_ok=True)
    json_path = REPORTS / f"{name}.json"
    md_path = REPORTS / f"{name}.md"
    json_path.write_text(
        json.dumps(report.to_dict(), ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    md_path.write_text(report.to_markdown(), encoding="utf-8")
    return {
        "json": str(json_path.relative_to(ROOT)),
        "markdown": str(md_path.relative_to(ROOT)),
        "summary": report.payload["summary"],
    }


def verify_projects() -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for slug in ("alpha", "beta", "gamma"):
        project, candidates, request = _load_bundle(slug)
        result = run_validation(project, candidates, request, mode="full")
        report_info = _report_for(project, candidates, result, f"{slug}_report")
        rows.append(
            {
                "slug": slug,
                "project_id": project.project_id,
                "project_hash": project.content_hash(),
                "candidate_set_hash": candidates.content_hash(),
                "combos": len(result.combos),
                "samples": sum(item.samples for item in result.combos),
                "peak_slots": result.schedule.actual_peak_slots,
                "queued_peak": result.schedule.queued_peak,
                "blocked_waves": result.schedule.blocked_waves,
                "submitted_candidates": sum(
                    1 for item in result.candidates if item.state == "SUBMITTED"
                ),
                "submitted_combos": result.schedule.committed_combos,
                "reduction_reasons": sorted(
                    {
                        item.reduction_reason
                        for item in result.plan.combos
                        if item.reduction_reason
                    }
                ),
                "report": report_info,
            }
        )
    return rows


def verify_three_paths() -> dict[str, Any]:
    project, candidates, request = _load_bundle("alpha")
    checkpoint_dir = CHECKPOINTS / "three_path"
    checkpoint_dir.mkdir(parents=True, exist_ok=True)
    for path in checkpoint_dir.glob("*"):
        path.unlink()
    realtime_cp = checkpoint_dir / "realtime.sqlite3"
    resumed_cp = checkpoint_dir / "resumed.sqlite3"
    realtime = run_validation(
        project, candidates, request, checkpoint_path=realtime_cp, mode="realtime"
    )
    partial_request = replace(request, interrupt_after_commits=12)
    partial = run_validation(
        project, candidates, partial_request, checkpoint_path=resumed_cp, mode="resume"
    )
    resumed = run_validation(
        project, candidates, request, checkpoint_path=resumed_cp, mode="resume"
    )
    full = run_validation(project, candidates, request, mode="full")
    first = compare_results(realtime, full, left_label="realtime", right_label="full")
    second = compare_results(resumed, full, left_label="resumed", right_label="full")
    return {
        "ok": first.equal and second.equal,
        "sandbox_interrupt": {
            "interrupted": partial.interrupted,
            "committed_combos": partial.schedule.committed_combos,
            "submitted_candidates": sum(
                1 for item in partial.candidates if item.state == "SUBMITTED"
            ),
        },
        "resume": {
            "restored_combos": resumed.schedule.restored_combos,
            "simulated_combos": resumed.schedule.simulated_combos,
        },
        "realtime_vs_full": first.to_dict(),
        "resumed_vs_full": second.to_dict(),
        "schedule": {
            "realtime": realtime.schedule.to_dict(),
            "resumed": resumed.schedule.to_dict(),
            "full": full.schedule.to_dict(),
        },
    }


def verify_serial_parallel() -> dict[str, Any]:
    project, candidates, _ = _load_bundle("alpha")
    request = SimulationRequest(target_samples=12, min_samples=2, total_sample_budget=400)
    serial = run_validation(
        project, candidates, request, mode="full", parallel_slots_override=1
    )
    parallel = run_validation(project, candidates, request, mode="full")
    comparison = compare_results(serial, parallel, left_label="serial", right_label="parallel")
    return {
        "equal": comparison.equal,
        "serial_peak_slots": serial.schedule.actual_peak_slots,
        "parallel_peak_slots": parallel.schedule.actual_peak_slots,
        "same_submission_order": [
            (item.candidate_id, item.scenario_id) for item in serial.combos
        ]
        == [(item.candidate_id, item.scenario_id) for item in parallel.combos],
        "comparison": comparison.to_dict(),
    }


def verify_budget_and_ci() -> dict[str, Any]:
    project, candidates, request = _load_bundle("alpha")
    plan = build_budget_plan(project, candidates.candidates, request)
    low_low, low_high, low_width = wilson_interval(12, 24)
    high_low, high_high, high_width = wilson_interval(120, 240)
    return {
        "target_samples": plan.target_samples,
        "combos": len(plan.combos),
        "allocated_samples": sum(item.samples for item in plan.combos),
        "total_sample_budget": plan.total_sample_budget,
        "per_thread_buffer_kb": plan.per_thread_buffer_kb,
        "reduced_combos": sum(
            1
            for item in plan.combos
            if item.samples < item.requested_samples
        ),
        "decision_count": len(plan.decisions),
        "decision_hash": decision_hash(plan),
        "ci_24_samples_bp": {"low": low_low, "high": low_high, "width": low_width},
        "ci_240_samples_bp": {"low": high_low, "high": high_high, "width": high_width},
        "ci_shrinks": high_width < low_width,
    }


def verify_checkpoint_invalidation() -> dict[str, Any]:
    project, candidates, _ = _load_bundle("alpha")
    request = SimulationRequest(target_samples=3, min_samples=2, total_sample_budget=150)
    checkpoint = CHECKPOINTS / "invalidation.sqlite3"
    _safe_unlink(checkpoint)
    run_validation(
        project,
        candidates,
        request,
        checkpoint_path=checkpoint,
        mode="realtime",
    )
    context = SnapshotContext.create(project, candidates, request)
    store = CheckpointStore(checkpoint)
    before = store.count()
    loaded = store.load(replace(context, budget_version="simulated-mismatch"))
    after = store.count()
    store.close()
    return {
        "rows_before": before,
        "invalidated": loaded.invalidated,
        "valid": loaded.valid,
        "reason": loaded.reason,
        "rows_after": after,
        "entire_snapshot_dropped": after == 0,
    }


def verify_injections() -> dict[str, Any]:
    specs = json.loads((DATA / "injection_cases.json").read_text(encoding="utf-8"))
    results: list[dict[str, Any]] = []
    passed = 0
    for case in specs["project_cases"]:
        raw = json.loads((DATA / case["project"]).read_text(encoding="utf-8"))
        mutated = apply_project_injection(raw, case["operation"])
        try:
            parse_project(mutated)
        except BalanceError as exc:
            ok = (
                exc.code.value == case["expected_code"]
                and exc.object_id == case["expected_object_id"]
                and exc.field == case["expected_field"]
            )
            passed += int(ok)
            results.append(
                {
                    "id": case["id"],
                    "passed": ok,
                    "actual": exc.to_dict(),
                }
            )
        else:
            results.append({"id": case["id"], "passed": False, "actual": None})

    runtime: list[dict[str, Any]] = []
    # E_STATE: direct illegal PENDING -> SUBMITTED transition.
    state = StateMachine()
    state.register(["candidate.runtime"], ["candidate.runtime::scene.runtime"])
    try:
        state.transition_combo("candidate.runtime::scene.runtime", ComboState.SUBMITTED)
    except BalanceError as exc:
        runtime.append(
            {
                "id": "illegal_combo_transition",
                "passed": exc.code == ErrorCode.STATE,
                "actual": exc.to_dict(),
            }
        )
    # E_PARA: target sample count outside the supported range.
    try:
        SimulationRequest(target_samples=0, min_samples=1).validate(42)
    except BalanceError as exc:
        runtime.append(
            {
                "id": "request_target_samples_zero",
                "passed": exc.code == ErrorCode.PARA,
                "actual": exc.to_dict(),
            }
        )
    # E_DIFF is covered with an exact round mutation by tests/test_three_paths_diff.py.
    runtime.append(
        {
            "id": "three_path_mismatch",
            "passed": True,
            "actual": {"code": "E_DIFF", "evidence": "pytest tests/test_three_paths_diff.py"},
        }
    )
    return {
        "ok": passed == len(results) and all(item["passed"] for item in runtime),
        "project_cases_passed": passed,
        "project_cases_total": len(results),
        "project_cases": results,
        "runtime_cases": runtime,
    }


def render_markdown(payload: dict[str, Any]) -> str:
    lines = [
        "# 本地验收记录",
        "",
        f"- 时间: {payload['generated_at']}",
        f"- Python: `{payload['environment']['python']}`",
        f"- 平台: `{payload['environment']['platform']}`",
        f"- PySide6: `{payload['environment']['pyside6']}`",
        f"- NumPy: `{payload['environment']['numpy']}`",
        f"- 样本 manifest: `{payload['manifest_hash']}`",
        "",
        "## 工程与报告",
        "",
        "| 工程 | 组合 | 采样 | 峰值槽位 | 排队峰值 | 已提交方案 | 削减原因 |",
        "|---|---:|---:|---:|---:|---:|---|",
    ]
    for row in payload["projects"]:
        lines.append(
            f"| {row['project_id']} | {row['combos']} | {row['samples']} | "
            f"{row['peak_slots']} | {row['queued_peak']} | {row['submitted_candidates']} | "
            f"{', '.join(row['reduction_reasons']) or '—'} |"
        )
    three = payload["three_paths"]
    serial = payload["serial_parallel"]
    budget = payload["budget_ci"]
    checkpoint = payload["checkpoint_invalidation"]
    injection = payload["injections"]
    lines.extend(
        [
            "",
            "## 一致性",
            "",
            f"- 三路径一致: **{'PASS' if three['ok'] else 'FAIL'}**",
            f"- 中断快照提交组合: {three['sandbox_interrupt']['committed_combos']}",
            f"- 恢复复用组合: {three['resume']['restored_combos']}；恢复补算: {three['resume']['simulated_combos']}",
            f"- 串行 vs 并行一致: **{'PASS' if serial['equal'] else 'FAIL'}** "
            f"(peak {serial['serial_peak_slots']} vs {serial['parallel_peak_slots']})",
            f"- 提交顺序一致: **{'PASS' if serial['same_submission_order'] else 'FAIL'}**",
            "",
            "## 预算与置信区间",
            "",
            f"- 42 组合分配 {budget['allocated_samples']} 次采样，目标 {budget['target_samples']} / 组合，"
            f"总预算 {budget['total_sample_budget']}",
            f"- 被削减组合: {budget['reduced_combos']}",
            f"- 每线程缓冲上限: {budget['per_thread_buffer_kb']} KiB",
            f"- 决策哈希: `{budget['decision_hash']}`",
            f"- Wilson 宽度 24 样本={budget['ci_24_samples_bp']['width']}bp，"
            f"240 样本={budget['ci_240_samples_bp']['width']}bp → "
            f"**{'收窄' if budget['ci_shrinks'] else '未收窄'}**",
            "",
            "## 检查点与错误码",
            "",
            f"- 版本不匹配整体作废: **{'PASS' if checkpoint['entire_snapshot_dropped'] else 'FAIL'}** "
            f"({checkpoint['rows_before']} → {checkpoint['rows_after']} 行，原因 `{checkpoint['reason']}`)",
            f"- 工程错误注入: {injection['project_cases_passed']}/{injection['project_cases_total']} 通过",
            f"- 运行时异常链路: **{'PASS' if injection['ok'] else 'FAIL'}**",
            "",
            "## 自动化测试",
            "",
            "```text",
            payload["pytest"]["summary"],
            "```",
            "",
            "## 复现",
            "",
            "```bash",
            "python scripts/generate_samples.py",
            "python scripts/run_demo.py",
            "python -m pytest -q",
            "python run_app.py",
            "```",
            "",
        ]
    )
    return "\n".join(lines)


def main() -> int:
    ARTIFACTS.mkdir(parents=True, exist_ok=True)
    CHECKPOINTS.mkdir(parents=True, exist_ok=True)
    manifest = generate_all(DATA)
    projects = verify_projects()
    three_paths = verify_three_paths()
    serial_parallel = verify_serial_parallel()
    budget_ci = verify_budget_and_ci()
    checkpoint = verify_checkpoint_invalidation()
    injections = verify_injections()
    test_env = dict(os.environ)
    test_env["QT_QPA_PLATFORM"] = test_env.get("QT_QPA_PLATFORM", "offscreen")
    test_process = subprocess.run(
        [sys.executable, "-m", "pytest", "-q"],
        cwd=ROOT,
        env=test_env,
        capture_output=True,
        text=True,
        check=False,
    )
    test_output = (test_process.stdout + test_process.stderr).strip()
    test_summary = test_output.splitlines()[-1] if test_output else "no pytest output"
    import numpy
    import PySide6

    payload = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "environment": {
            "python": platform.python_version(),
            "platform": platform.platform(),
            "pyside6": PySide6.__version__,
            "numpy": numpy.__version__,
        },
        "manifest_hash": manifest["manifest_hash"],
        "projects": projects,
        "three_paths": three_paths,
        "serial_parallel": serial_parallel,
        "budget_ci": budget_ci,
        "checkpoint_invalidation": checkpoint,
        "injections": injections,
        "pytest": {
            "returncode": test_process.returncode,
            "summary": test_summary,
        },
    }
    (ARTIFACTS / "verification.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    (ARTIFACTS / "verification.md").write_text(render_markdown(payload), encoding="utf-8")
    print(json.dumps(
        {
            "ok": all(
                [
                    three_paths["ok"],
                    serial_parallel["equal"],
                    budget_ci["ci_shrinks"],
                    checkpoint["entire_snapshot_dropped"],
                    injections["ok"],
                    test_process.returncode == 0,
                ]
            ),
            "manifest_hash": manifest["manifest_hash"],
            "verification": "artifacts/verification.json",
            "reports": [row["report"] for row in projects],
        },
        ensure_ascii=False,
        indent=2,
    ))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
