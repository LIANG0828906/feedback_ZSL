"""End-to-end local acceptance checks reported as machine-readable JSON."""

from __future__ import annotations

import argparse
import copy
import json
import os
import platform
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .errors import WorkbenchError
from .filters import STAGE_FUNCTIONS
from .imaging import decode_image
from .pipeline import PipelineSession, RunControl
from .samples import generate_samples, load_manifest
from .snapshot import SnapshotStore
from .stages import DEFAULT_PARAMS, STAGE_IDS, normalize_pipeline_params


PROJECT_ROOT = Path(__file__).resolve().parent.parent


def _check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def _record(
    checks: list[dict[str, Any]],
    name: str,
    action: Callable[[], Any],
) -> Any:
    started = time.perf_counter()
    detail: Any = None
    try:
        detail = action()
        checks.append(
            {
                "name": name,
                "ok": True,
                "elapsed_sec": round(time.perf_counter() - started, 6),
                "detail": detail,
            }
        )
        return detail
    except Exception as exc:
        checks.append(
            {
                "name": name,
                "ok": False,
                "elapsed_sec": round(time.perf_counter() - started, 6),
                "error": f"{type(exc).__name__}: {exc}",
            }
        )
        return None


def _scenario_params(sequences: dict[str, Any], name: str) -> dict[str, Any]:
    for scenario in sequences["scenarios"]:
        if scenario["name"] == name:
            return copy.deepcopy(scenario["params"])
    raise KeyError(f"缺少参数场景: {name}")


def _stats_dump(run) -> dict[str, Any]:
    return {
        "status": run.status,
        "path_type": run.diagnostics.path_type,
        "source": run.diagnostics.source,
        "recompute_count": run.diagnostics.recompute_count,
        "cache_hit_count": run.diagnostics.cache_hit_count,
        "effective_cache_entries": run.diagnostics.effective_cache_entries,
        "full_stage_count": run.diagnostics.full_stage_count,
        "total_elapsed_sec": round(run.diagnostics.total_elapsed_sec, 6),
        "stages": [
            {
                "stage": stage.stage,
                "recomputes": stage.recomputes,
                "hits": stage.hits,
                "elapsed_sec": round(stage.elapsed_sec, 6),
            }
            for stage in run.diagnostics.stages
        ],
    }


def _verify_sample(
    sample_dir: Path,
    sample: dict[str, Any],
    params_sequences: dict[str, Any],
    work_root: Path,
    index: int,
) -> dict[str, Any]:
    image_path = sample_dir / sample["file"]
    snapshot_path = work_root / f"sample_{index}" / "pipeline.snapshot"
    session = PipelineSession(snapshot_path)
    restore = session.set_image(image_path)
    _check(restore.status == "none", f"新图像应无快照，实际 {restore.status}")
    baseline = session.run()
    _check(baseline.status == "complete", f"基线失败: {baseline.error}")
    _check(baseline.diagnostics.path_type == "full", "首次运行应为 full")
    _check(baseline.diagnostics.recompute_count == 3, "首次运行应重算 3 阶段")
    _check(baseline.output.shape == session.image.array.shape, "输出尺寸改变")
    _check(baseline.output.dtype == np.uint8, "输出 dtype 不是 uint8")
    baseline_audit = session.compare_with_full()
    _check(baseline_audit.equal, baseline_audit.message)

    old_entries = {
        stage_id: session.cache.get(stage_id) for stage_id in STAGE_IDS
    }
    contrast_params = _scenario_params(params_sequences, "contrast_only")
    affected_contrast = session.set_pipeline_params(contrast_params)
    _check(affected_contrast == ("local_contrast",), f"失效边界错误: {affected_contrast}")
    _check(
        session.cache.get("denoise") is old_entries["denoise"]
        and session.cache.get("sharpen") is old_entries["sharpen"],
        "末端参数变化不应清除上游缓存",
    )
    contrast_run = session.run()
    _check(contrast_run.status == "complete", f"末端增量失败: {contrast_run.error}")
    _check(contrast_run.diagnostics.recompute_count == 1, "末端增量应只重算 1 阶段")
    _check(contrast_run.diagnostics.cache_hit_count == 2, "末端增量应命中 2 阶段")
    _check(
        contrast_run.diagnostics.effective_cache_entries
        <= contrast_run.diagnostics.full_stage_count,
        "有效缓存数超过全量阶段数",
    )
    contrast_audit = session.compare_with_full()
    _check(contrast_audit.equal, contrast_audit.message)

    sharpen_params = _scenario_params(params_sequences, "sharpen_and_downstream")
    affected_sharpen = session.set_pipeline_params(sharpen_params)
    _check(
        affected_sharpen == ("sharpen", "local_contrast"),
        f"锐化失效边界错误: {affected_sharpen}",
    )
    _check(session.cache.get("denoise") is old_entries["denoise"], "去噪缓存被误删")
    sharpen_run = session.run()
    _check(sharpen_run.status == "complete", f"锐化增量失败: {sharpen_run.error}")
    _check(sharpen_run.diagnostics.recompute_count == 2, "锐化增量应重算 2 阶段")
    _check(sharpen_run.diagnostics.cache_hit_count == 1, "锐化增量应命中 1 阶段")
    sharpen_audit = session.compare_with_full()
    _check(sharpen_audit.equal, sharpen_audit.message)
    _check(
        sharpen_audit.incremental_cache_entries
        <= sharpen_audit.full_stage_count,
        "增量有效缓存数大于全量阶段数",
    )

    manifest = SnapshotStore(snapshot_path).peek_manifest()
    _check(manifest is not None, "快照清单不可读")
    _check(manifest["chain_complete"] is True, "完整链未标记完成")
    _check(len(manifest["completed_stages"]) == 3, "快照缺少阶段输出")

    restored = PipelineSession(snapshot_path, params=sharpen_params)
    restore_again = restored.set_image(image_path)
    _check(restore_again.status == "restored", f"重启恢复失败: {restore_again.reason}")
    _check(len(restored.cache) == 3, "恢复缓存不是 3 阶段")
    restored_run = restored.run()
    _check(restored_run.diagnostics.recompute_count == 0, "恢复后不应重算")
    _check(restored_run.diagnostics.cache_hit_count == 3, "恢复后应全部命中")
    _check(restored.compare_with_full().equal, "恢复路径与全量不一致")

    return {
        "file": sample["file"],
        "mode": sample["mode"],
        "width": sample["width"],
        "height": sample["height"],
        "channels": sample["channels"],
        "output_shape": list(baseline.output.shape),
        "baseline": _stats_dump(baseline),
        "contrast_only_incremental": _stats_dump(contrast_run),
        "sharpen_incremental": _stats_dump(sharpen_run),
        "restore": {
            "status": restore_again.status,
            "loaded_stages": list(restore_again.loaded_stages),
            "recompute_count": restored_run.diagnostics.recompute_count,
            "cache_hit_count": restored_run.diagnostics.cache_hit_count,
        },
        "incremental_equals_full": True,
    }


def _verify_parameter_and_decode_errors(
    sample_dir: Path,
    sample: dict[str, Any],
    work_root: Path,
) -> dict[str, Any]:
    invalid = copy.deepcopy(DEFAULT_PARAMS)
    invalid["local_contrast"]["clip_limit"] = 99.0
    try:
        normalize_pipeline_params(invalid)
    except WorkbenchError as exc:
        _check(exc.code == "E_PARA", "越界参数未返回 E_PARA")
        _check(exc.stage == "local_contrast", "E_PARA 阶段名错误")
        _check(exc.param == "clip_limit", "E_PARA 参数名错误")
        para_error = exc.to_dict()
    else:
        raise AssertionError("越界参数未被拒绝")

    bad_path = work_root / "corrupt_input.png"
    bad_path.write_bytes(b"\x89PNG\r\n\x1a\ncorrupt")
    try:
        decode_image(bad_path)
    except WorkbenchError as exc:
        _check(exc.code == "E_DECODE", "损坏图像未返回 E_DECODE")
        decode_error = exc.to_dict()
    else:
        raise AssertionError("损坏图像未被拒绝")

    image_path = sample_dir / sample["file"]
    mismatched = copy.deepcopy(DEFAULT_PARAMS)
    mismatched["local_contrast"]["blend"] = 0.51
    snapshot_path = work_root / "param-mismatch" / "pipeline.snapshot"
    session = PipelineSession(snapshot_path)
    session.set_image(image_path)
    session.run()
    stale = PipelineSession(snapshot_path, params=mismatched)
    restore = stale.set_image(image_path)
    _check(restore.status == "discarded", "参数不匹配快照未整体作废")
    _check(len(stale.cache) == 0, "参数不匹配后仍残留缓存")
    _check(not snapshot_path.exists(), "作废快照未被删除")
    rerun = stale.run()
    _check(rerun.diagnostics.recompute_count == 3, "作废后未全量重算")

    return {
        "e_para": para_error,
        "e_decode": decode_error,
        "parameter_mismatch_restore": restore.to_dict(),
        "parameter_mismatch_rerun": _stats_dump(rerun),
    }


def _verify_cancel_and_retry(
    sample_dir: Path,
    sample: dict[str, Any],
    work_root: Path,
) -> dict[str, Any]:
    image_path = sample_dir / sample["file"]
    cancel_snapshot = work_root / "cancel" / "pipeline.snapshot"
    session = PipelineSession(cancel_snapshot)
    session.set_image(image_path)
    control = RunControl()

    def listener(event: dict[str, Any]) -> None:
        if event.get("type") == "stage_done" and event.get("stage") == "denoise":
            control.cancel()

    cancelled = session.run(control=control, listener=listener)
    _check(cancelled.status == "cancelled", "取消状态未生效")
    _check(list(session.cache.entries) == ["denoise"], "取消后已提交前缀不正确")
    manifest = session.snapshot.peek_manifest()
    _check(manifest["chain_complete"] is False, "取消快照不应标记完整")
    _check(manifest["completed_stages"] == ["denoise"], "取消快照阶段不匹配")

    resumed = PipelineSession(cancel_snapshot)
    restore = resumed.set_image(image_path)
    _check(restore.status == "partial", "取消快照未按部分恢复")
    resumed_run = resumed.run()
    _check(resumed_run.status == "complete", "取消后恢复未完成")
    _check(resumed_run.diagnostics.recompute_count == 2, "取消恢复重算数错误")
    _check(resumed_run.diagnostics.cache_hit_count == 1, "取消恢复命中数错误")
    _check(resumed.compare_with_full().equal, "取消恢复结果与全量不一致")

    fail_state = {"fail": True}

    def flaky_runner(stage_id, array, params, checkpoint):
        if stage_id == "sharpen" and fail_state["fail"]:
            fail_state["fail"] = False
            raise RuntimeError("injected sharpen failure")
        return STAGE_FUNCTIONS[stage_id](array, params, checkpoint)

    retry_snapshot = work_root / "retry" / "pipeline.snapshot"
    failing = PipelineSession(retry_snapshot, stage_runner=flaky_runner)
    failing.set_image(image_path)
    failed = failing.run()
    _check(failed.status == "failed", "注入阶段失败未生效")
    _check(failed.error["code"] == "E_STAGE", "阶段失败代码错误")
    _check(list(failing.cache.entries) == ["denoise"], "失败污染上游/下游缓存")
    retried = failing.retry()
    _check(retried.status == "complete", "失败阶段重试未完成")
    _check(failing.compare_with_full().equal, "重试结果与全量不一致")

    return {
        "cancelled": {
            "status": cancelled.status,
            "completed_stages": list(cancelled.completed_stages),
            "snapshot_completed_stages": manifest["completed_stages"],
        },
        "resumed": {
            "restore_status": restore.status,
            "recompute_count": resumed_run.diagnostics.recompute_count,
            "cache_hit_count": resumed_run.diagnostics.cache_hit_count,
            "consistent": True,
        },
        "failed_stage_retry": {
            "failure_code": failed.error["code"],
            "failure_stage": failed.error["stage"],
            "cached_after_failure": ["denoise"],
            "retry_status": retried.status,
            "retry_recompute_count": retried.diagnostics.recompute_count,
            "consistent": True,
        },
    }


def _hard_exit_child(
    snapshot_path: Path,
    image_path: Path,
    params: dict[str, Any],
) -> subprocess.CompletedProcess[str]:
    code = r"""
import json
import os
import sys
from filter_studio.pipeline import PipelineSession

snapshot_path, image_path, params_json = sys.argv[1:4]
session = PipelineSession(snapshot_path, params=json.loads(params_json))
session.set_image(image_path)

def listener(event):
    if event.get("type") == "stage_done" and event.get("stage") == "denoise":
        os._exit(37)

session.run(listener=listener)
"""
    environment = os.environ.copy()
    environment["PYTHONPATH"] = str(PROJECT_ROOT)
    return subprocess.run(
        [
            sys.executable,
            "-c",
            code,
            str(snapshot_path),
            str(image_path),
            json.dumps(params, ensure_ascii=False),
        ],
        cwd=PROJECT_ROOT,
        env=environment,
        capture_output=True,
        text=True,
        timeout=60,
    )


def _verify_hard_exit(
    sample_dir: Path,
    sample: dict[str, Any],
    work_root: Path,
) -> dict[str, Any]:
    snapshot_path = work_root / "hard-exit" / "pipeline.snapshot"
    image_path = sample_dir / sample["file"]
    process = _hard_exit_child(snapshot_path, image_path, DEFAULT_PARAMS)
    _check(process.returncode == 37, f"硬终止子进程退出码异常: {process.returncode}")
    manifest = SnapshotStore(snapshot_path).peek_manifest()
    _check(manifest is not None, "硬终止后没有可读快照")
    _check(manifest["chain_complete"] is False, "硬终止快照不应完整")
    _check(manifest["completed_stages"] == ["denoise"], "硬终止前缀不正确")

    recovered = PipelineSession(snapshot_path)
    restore = recovered.set_image(image_path)
    _check(restore.status == "partial", "硬终止后未按部分快照恢复")
    _check(recovered.last_run is None, "恢复前不应存在完整处理报告")
    resumed = recovered.run()
    _check(resumed.status == "complete", "硬终止后继续处理失败")
    _check(any("不对强制终止前的完整报告作任何声明" in note for note in resumed.diagnostics.notes), "缺少强制终止声明")
    audit = recovered.compare_with_full()
    _check(audit.equal, "硬终止恢复结果与全量不一致")
    return {
        "child_exit_code": process.returncode,
        "snapshot_chain_complete": manifest["chain_complete"],
        "persisted_prefix": manifest["completed_stages"],
        "restore_status": restore.status,
        "pre_resume_report": None,
        "resume_recompute_count": resumed.diagnostics.recompute_count,
        "resume_cache_hit_count": resumed.diagnostics.cache_hit_count,
        "consistent": True,
        "disclaimer_present": True,
    }


def _verify_e_diff(
    sample_dir: Path,
    sample: dict[str, Any],
    work_root: Path,
) -> dict[str, Any]:
    calls = {"local": 0}

    def flaky_runner(stage_id, array, params, checkpoint):
        output = STAGE_FUNCTIONS[stage_id](array, params, checkpoint)
        if stage_id == "local_contrast":
            calls["local"] += 1
            if calls["local"] == 1:
                output = output.copy()
                output[0, 0] = np.uint8((int(output[0, 0]) + 1) % 256)
        return output

    session = PipelineSession(
        work_root / "e-diff" / "pipeline.snapshot",
        stage_runner=flaky_runner,
    )
    session.set_image(sample_dir / sample["file"])
    session.run()
    report = session.compare_with_full()
    _check(not report.equal, "注入差异后仍报告一致")
    _check(report.code == "E_DIFF", "差异未返回 E_DIFF")
    _check(report.stage == "local_contrast", "差异阶段定位错误")
    _check((report.x, report.y, report.channel) == (0, 0, 0), "首差异坐标错误")
    return report.to_dict() | {"full_diagnostics": None}


def _verify_ui_smoke(
    sample_dir: Path,
    work_root: Path,
    screenshot_path: Path,
) -> dict[str, Any]:
    code = r"""
import os
import sys
import time
os.environ["QT_QPA_PLATFORM"] = "offscreen"
from PySide6.QtWidgets import QApplication, QTabWidget
from filter_studio.ui.main import MainWindow, _configure_application

sample_dir, cache_dir, screenshot_path = sys.argv[1:4]
app = QApplication([])
_configure_application(app)
window = MainWindow(sample_dir, cache_dir, auto_start=False)
window.show()
app.processEvents()
window._start_pipeline(force_full=False)
deadline = time.monotonic() + 30
while window._busy and time.monotonic() < deadline:
    app.processEvents()
    time.sleep(0.01)
app.processEvents()
assert window._last_run is not None and window._last_run.status == "complete"
assert window.cache_table.rowCount() == 3
assert window._last_cached_output() is not None
window.grab().save(screenshot_path)
dependency_path = os.path.join(os.path.dirname(screenshot_path), "ui_dependency.png")
diagnostics_path = os.path.join(os.path.dirname(screenshot_path), "ui_diagnostics.png")
tabs = window.findChild(QTabWidget)
tabs.setCurrentIndex(1)
app.processEvents()
window.grab().save(dependency_path)
tabs.setCurrentIndex(2)
app.processEvents()
window.grab().save(diagnostics_path)
window.close()
app.processEvents()
"""
    environment = os.environ.copy()
    environment["PYTHONPATH"] = str(PROJECT_ROOT)
    environment["QT_QPA_PLATFORM"] = "offscreen"
    process = subprocess.run(
        [
            sys.executable,
            "-c",
            code,
            str(sample_dir),
            str(work_root / "ui-cache"),
            str(screenshot_path),
        ],
        cwd=PROJECT_ROOT,
        env=environment,
        capture_output=True,
        text=True,
        timeout=60,
    )
    _check(
        process.returncode == 0,
        f"UI 冒烟失败: {process.stderr[-2000:]}",
    )
    _check(screenshot_path.is_file(), "UI 截图未生成")
    dependency_path = screenshot_path.with_name("ui_dependency.png")
    diagnostics_path = screenshot_path.with_name("ui_diagnostics.png")
    _check(dependency_path.is_file(), "依赖图截图未生成")
    _check(diagnostics_path.is_file(), "诊断面板截图未生成")
    return {
        "offscreen_startup": True,
        "cache_rows": 3,
        "screenshots": [
            str(screenshot_path),
            str(dependency_path),
            str(diagnostics_path),
        ],
        "stdout": process.stdout.strip(),
    }


def run_acceptance(
    *,
    samples_dir: str | Path = "samples",
    report_path: str | Path = "artifacts/acceptance_report.json",
    screenshot_path: str | Path = "artifacts/ui_smoke.png",
    skip_ui: bool = False,
) -> dict[str, Any]:
    sample_dir = Path(samples_dir)
    if not (sample_dir / "manifest.json").is_file():
        generate_samples(sample_dir)
    manifest = load_manifest(sample_dir)
    sequences = json.loads(
        (sample_dir / "params_sequences.json").read_text(encoding="utf-8")
    )
    checks: list[dict[str, Any]] = []
    samples_report: list[dict[str, Any]] = []
    with tempfile.TemporaryDirectory(prefix="lumen-acceptance-") as temporary:
        work_root = Path(temporary)
        for index, sample in enumerate(manifest["samples"]):
            result = _record(
                checks,
                f"sample:{sample['file']}",
                lambda index=index, sample=sample: _verify_sample(
                    sample_dir,
                    sample,
                    sequences,
                    work_root,
                    index,
                ),
            )
            if result is not None:
                samples_report.append(result)

        first = manifest["samples"][0]
        second = manifest["samples"][-1]
        errors = _record(
            checks,
            "errors-and-parameter-invalidation",
            lambda: _verify_parameter_and_decode_errors(
                sample_dir, first, work_root
            ),
        )
        interruption = _record(
            checks,
            "cancel-failure-retry",
            lambda: _verify_cancel_and_retry(sample_dir, first, work_root),
        )
        hard_exit = _record(
            checks,
            "hard-process-exit-recovery",
            lambda: _verify_hard_exit(sample_dir, first, work_root),
        )
        diff = _record(
            checks,
            "e-diff-first-pixel",
            lambda: _verify_e_diff(sample_dir, first, work_root),
        )
        input_change = _record(
            checks,
            "input-change-invalidates-all",
            lambda: _verify_input_change(
                sample_dir, first, second, work_root
            ),
        )
        if not skip_ui:
            screenshot = Path(screenshot_path)
            screenshot.parent.mkdir(parents=True, exist_ok=True)
            ui = _record(
                checks,
                "pyside6-offscreen-smoke",
                lambda: _verify_ui_smoke(sample_dir, work_root, screenshot),
            )
        else:
            ui = None

    passed = all(check["ok"] for check in checks)
    report = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "environment": {
            "python": sys.version,
            "platform": platform.platform(),
            "executable": sys.executable,
            "numpy": np.__version__,
            "pillow": __import__("PIL").__version__,
            "pyside6": __import__("PySide6").__version__,
            "pip_available": _pip_available(),
        },
        "samples": samples_report,
        "errors": errors,
        "interruption": interruption,
        "hard_exit_recovery": hard_exit,
        "e_diff": diff,
        "input_change": input_change,
        "ui_smoke": ui,
        "checks": checks,
        "summary": {
            "passed": sum(1 for check in checks if check["ok"]),
            "failed": sum(1 for check in checks if not check["ok"]),
            "all_passed": passed,
        },
    }
    destination = Path(report_path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(f".{destination.name}.tmp")
    temporary.write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True),
        encoding="utf-8",
    )
    temporary.replace(destination)
    return report


def _verify_input_change(
    sample_dir: Path,
    first: dict[str, Any],
    second: dict[str, Any],
    work_root: Path,
) -> dict[str, Any]:
    snapshot_path = work_root / "input-change" / "pipeline.snapshot"
    session = PipelineSession(snapshot_path)
    session.set_image(sample_dir / first["file"])
    session.run()
    old_hash = session.image.content_hash
    restore = session.set_image(sample_dir / second["file"])
    _check(restore.status == "none", "输入变化后不应恢复旧快照")
    _check(len(session.cache) == 0, "输入变化后缓存未整体清空")
    _check(not snapshot_path.exists(), "输入变化后旧快照未删除")
    _check(session.image.content_hash != old_hash, "输入内容哈希未变化")
    return {
        "restore_status": restore.status,
        "cache_entries_after_change": 0,
        "snapshot_exists": snapshot_path.exists(),
        "new_input_hash": session.image.content_hash,
    }


def _pip_available() -> bool:
    try:
        result = subprocess.run(
            [sys.executable, "-m", "pip", "--version"],
            capture_output=True,
            text=True,
            timeout=10,
        )
        return result.returncode == 0
    except Exception:
        return False


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run Lumen acceptance checks")
    parser.add_argument("--samples", default="samples")
    parser.add_argument("--report", default="artifacts/acceptance_report.json")
    parser.add_argument("--screenshot", default="artifacts/ui_smoke.png")
    parser.add_argument("--skip-ui", action="store_true")
    args = parser.parse_args(argv)
    report = run_acceptance(
        samples_dir=args.samples,
        report_path=args.report,
        screenshot_path=args.screenshot,
        skip_ui=args.skip_ui,
    )
    summary = report["summary"]
    print(
        f"acceptance: {summary['passed']} passed, {summary['failed']} failed; "
        f"report={args.report}"
    )
    for check in report["checks"]:
        marker = "PASS" if check["ok"] else "FAIL"
        print(f"[{marker}] {check['name']} ({check['elapsed_sec']:.3f}s)")
        if not check["ok"]:
            print(f"       {check['error']}")
    return 0 if summary["all_passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

