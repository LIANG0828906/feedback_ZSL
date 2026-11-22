#!/usr/bin/env python3
"""Run the complete local acceptance suite and write reproducible evidence."""

from __future__ import annotations

import json
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
PYTHON = sys.executable


def cli(args: list[str], *, expected_code: int = 0) -> dict[str, Any]:
    started = time.perf_counter()
    completed = subprocess.run(
        [PYTHON, "-m", "mshdr", *args],
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=180,
    )
    elapsed = time.perf_counter() - started
    if completed.returncode != expected_code:
        raise RuntimeError(
            f"Command failed ({completed.returncode} != {expected_code}): mshdr {' '.join(args)}\n"
            f"stdout:\n{completed.stdout}\nstderr:\n{completed.stderr}"
        )
    payload: dict[str, Any] = {}
    raw = completed.stdout.strip() or completed.stderr.strip()
    if raw:
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError:
            payload = {"raw_output": raw}
    return {
        "command": f"mshdr {' '.join(args)}",
        "returncode": completed.returncode,
        "elapsed_seconds": round(elapsed, 4),
        "payload": payload,
    }


def unit_tests() -> dict[str, Any]:
    started = time.perf_counter()
    completed = subprocess.run(
        [PYTHON, "-m", "unittest", "discover", "-s", "tests", "-v"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        timeout=180,
    )
    if completed.returncode != 0:
        raise RuntimeError(f"Unit tests failed:\n{completed.stdout}\n{completed.stderr}")
    lines = [line for line in (completed.stdout + completed.stderr).splitlines() if line.strip()]
    summary = lines[-3:] if len(lines) >= 3 else lines
    return {
        "returncode": completed.returncode,
        "elapsed_seconds": round(time.perf_counter() - started, 4),
        "summary": summary,
    }


def main() -> int:
    report: dict[str, Any] = {
        "ok": False,
        "started_at_utc": datetime.now(timezone.utc).isoformat(),
        "python": sys.version,
        "project_root": str(ROOT),
        "steps": {},
    }
    try:
        report["unit_tests"] = unit_tests()
        report["steps"]["doctor"] = cli(["doctor"])
        report["steps"]["generate"] = cli(["generate", "--seed", "20261123", "--out", "samples"])
        report["steps"]["inspect"] = cli(["inspect", "--dataset", "samples/dataset.json"])
        report["steps"]["three_paths"] = cli(
            [
                "run-paths",
                "--dataset",
                "samples/dataset.json",
                "--out",
                "outputs/paths",
                "--checkpoint-root",
                "checkpoints/paths",
                "--workers",
                "4",
            ]
        )

        anomaly_dir = ROOT / "outputs" / "acceptance" / "injected"
        anomalies = (
            ("missing_band", 2, "E_META"),
            ("exposure_order", 9, "E_ORDER"),
            ("bit_depth_change", 8, "E_VER"),
        )
        report["anomalies"] = {}
        for kind, exit_code, error_code in anomalies:
            destination = anomaly_dir / kind
            cli(
                [
                    "inject",
                    "--source",
                    "samples/dataset.json",
                    "--destination",
                    str(destination),
                    "--kind",
                    kind,
                    "--scene",
                    "scene_00_neon_dusk",
                ]
            )
            result = cli(
                [
                    "run",
                    "--dataset",
                    str(destination / "dataset.json"),
                    "--scene",
                    "scene_00_neon_dusk",
                    "--start-mode",
                    "none",
                    "--output",
                    str(ROOT / "outputs" / "acceptance" / f"anomaly_{kind}"),
                ],
                expected_code=exit_code,
            )
            actual_code = result["payload"].get("code")
            if actual_code != error_code:
                raise RuntimeError(f"{kind}: expected {error_code}, got {actual_code}: {result['payload']}")
            report["anomalies"][kind] = result

        bad_checkpoint = ROOT / "checkpoints" / "acceptance_bad_segment"
        bad_output = ROOT / "outputs" / "acceptance" / "bad_segment_retry"
        report["bad_segment_failure"] = cli(
            [
                "run",
                "--dataset",
                "samples/dataset.json",
                "--scene",
                "scene_00_neon_dusk",
                "--start-mode",
                "fresh",
                "--checkpoint-root",
                str(bad_checkpoint),
                "--output",
                str(bad_output),
                "--bad-segment",
                "5",
            ],
            expected_code=10,
        )
        if report["bad_segment_failure"]["payload"].get("code") != "E_SEG":
            raise RuntimeError("Injected bad segment did not return E_SEG")
        report["bad_segment_retry"] = cli(
            [
                "run",
                "--dataset",
                "samples/dataset.json",
                "--scene",
                "scene_00_neon_dusk",
                "--start-mode",
                "resume",
                "--checkpoint-root",
                str(bad_checkpoint),
                "--output",
                str(bad_output),
            ]
        )
        report["bad_segment_comparison"] = cli(
            [
                "compare",
                "--left",
                "outputs/paths/full/scene_00_neon_dusk",
                "--right",
                str(bad_output),
                "--report",
                str(ROOT / "outputs" / "acceptance" / "bad_segment_comparison.json"),
            ]
        )

        crash_checkpoint = ROOT / "checkpoints" / "acceptance_crash"
        crash_output = ROOT / "outputs" / "acceptance" / "crash_resume"
        report["forced_exit"] = cli(
            [
                "run",
                "--dataset",
                "samples/dataset.json",
                "--scene",
                "scene_00_neon_dusk",
                "--start-mode",
                "fresh",
                "--checkpoint-root",
                str(crash_checkpoint),
                "--crash-after-commits",
                "4",
            ],
            expected_code=91,
        )
        report["forced_resume"] = cli(
            [
                "run",
                "--dataset",
                "samples/dataset.json",
                "--scene",
                "scene_00_neon_dusk",
                "--start-mode",
                "resume",
                "--checkpoint-root",
                str(crash_checkpoint),
                "--output",
                str(crash_output),
            ]
        )
        report["forced_resume_comparison"] = cli(
            [
                "compare",
                "--left",
                "outputs/paths/full/scene_00_neon_dusk",
                "--right",
                str(crash_output),
                "--report",
                str(ROOT / "outputs" / "acceptance" / "forced_resume_comparison.json"),
            ]
        )
        report["ok"] = True
    except Exception as exc:
        report["error"] = f"{type(exc).__name__}: {exc}"
        raise
    finally:
        report["finished_at_utc"] = datetime.now(timezone.utc).isoformat()
        destination = ROOT / "outputs" / "acceptance" / "acceptance_report.json"
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(
            json.dumps(report, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
        if report.get("ok"):
            write_validation_markdown(report)
    print(json.dumps(report, indent=2, sort_keys=True, ensure_ascii=False))
    return 0 if report.get("ok") else 1


def write_validation_markdown(report: dict[str, Any]) -> None:
    paths_report = report["steps"]["three_paths"]["payload"]
    lines = [
        "# 实际验证记录",
        "",
        f"- 生成时间（UTC）：`{report['finished_at_utc']}`",
        f"- Python：`{report['python'].splitlines()[0]}`",
        f"- 自动化测试：`{report['unit_tests']['summary'][-1] if report['unit_tests']['summary'] else 'OK'}`",
        "- 运行环境：本地 CPU，无网络、GPU 或外部服务。",
        "",
        "## 三路径逐像素一致性",
        "",
        "| 场景 | full vs realtime | full vs resume | realtime vs resume | 残留标准差 | 边缘保真度 | 越界像素 |",
        "|---|---:|---:|---:|---:|---:|---:|",
    ]
    for scene in paths_report["scenes"]:
        comparisons = scene["comparisons"]
        lines.append(
            f"| {scene['scene_id']} | {comparisons['full_vs_realtime']} | "
            f"{comparisons['full_vs_resume']} | {comparisons['realtime_vs_resume']} | "
            f"{scene['residual_std']:.8f} | {scene['edge_fidelity']:.8f} | {scene['out_of_gamut_count']} |"
        )
    lines.extend(
        [
            "",
            "比对采用 `np.array_equal` 严格相等，不设浮点容差；首个差异会定位到像素坐标、分段编号与字段名。",
            "",
            "## 异常注入链路",
            "",
            "| 注入 | 预期错误码 | 实际错误码 | 结果 |",
            "|---|---|---|---|",
        ]
    )
    for kind, result in report["anomalies"].items():
        payload = result["payload"]
        expected = {"missing_band": "E_META", "exposure_order": "E_ORDER", "bit_depth_change": "E_VER"}[kind]
        lines.append(f"| {kind} | {expected} | {payload.get('code')} | 通过 |")
    lines.extend(
        [
            f"| bad_segment | E_SEG | {report['bad_segment_failure']['payload'].get('code')} | 通过，随后断点重试 |",
            "",
            "## 强制终止恢复",
            "",
            f"- 第 4 段提交后以退出码 `91` 强制终止：{report['forced_exit']['returncode']}",
            f"- 断点恢复完成并输出：{report['forced_resume']['payload'].get('progress')}",
            f"- 恢复结果与 full 路径严格比较：{report['forced_resume_comparison']['payload'].get('ok')}",
            f"- 坏段重试结果与 full 路径严格比较：{report['bad_segment_comparison']['payload'].get('ok')}",
            "",
            "## 证据文件",
            "",
            "- `outputs/acceptance/acceptance_report.json`",
            "- `outputs/paths/paths_report.json`",
            "- `outputs/paths/comparisons/*/*.json`",
            "- `outputs/paths/full/*/stats.json`",
            "- `outputs/acceptance/bad_segment_comparison.json`",
            "- `outputs/acceptance/forced_resume_comparison.json`",
            "",
        ]
    )
    path = ROOT / "docs" / "VALIDATION.md"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    raise SystemExit(main())

