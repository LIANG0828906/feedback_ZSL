from __future__ import annotations

import argparse
import json
from pathlib import Path
import shutil
import sys
from typing import Any

from .config import PipelineConfig
from .errors import EdgeForgeError
from .loader import load_sample
from .pipeline import Pipeline
from .samples import DEFAULT_SAMPLE_IDS, generate_samples
from .verification import run_verification


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="edgeforge",
        description="游戏与多媒体素材边缘检测/形态学处理桌面工具",
    )
    parser.add_argument("--version", action="version", version="edgeforge 1.0")
    subparsers = parser.add_subparsers(dest="command", required=True)

    samples = subparsers.add_parser("samples", help="生成固定种子本地样本")
    samples.add_argument("--out", default="samples", help="输出目录")
    samples.add_argument(
        "--id",
        action="append",
        dest="sample_ids",
        help="仅生成指定样本，可重复",
    )
    samples.add_argument("--json", action="store_true", help="输出 manifest JSON")

    run = subparsers.add_parser("run", help="运行分块管线")
    run.add_argument("--samples", default="samples")
    run.add_argument("--sample", default="low_contrast_gray_320x240")
    run.add_argument("--run-dir", default=None)
    run.add_argument("--mode", choices=["serial", "parallel"], default="parallel")
    run.add_argument("--tile", type=int, default=192)
    run.add_argument("--parallel", type=int, default=4)
    run.add_argument("--stages", type=int, default=6)
    run.add_argument("--low", type=float, default=8.0)
    run.add_argument("--high", type=float, default=18.0)
    run.add_argument("--open-size", type=int, default=3)
    run.add_argument("--close-size", type=int, default=3)
    run.add_argument("--thin", type=int, default=8)
    run.add_argument("--prune", type=int, default=6)
    run.add_argument("--band", type=int, default=None)
    run.add_argument("--memory-mb", type=int, default=64)
    run.add_argument("--thread-pixels", type=int, default=1_500_000)
    run.add_argument("--fault", default=None, help="stage:block_id，单次故障注入")
    run.add_argument("--retry", action="store_true", help="重试失败的分块阶段")

    verify = subparsers.add_parser("verify", help="运行端到端确定性与异常码验证")
    verify.add_argument("--samples", default=None)
    verify.add_argument("--work-dir", default=None)
    verify.add_argument("--json-out", default=None)

    gui = subparsers.add_parser("gui", help="启动 PyQt5 诊断界面")
    gui.add_argument("--samples", default="samples")
    gui.add_argument("--run-dir", default="runs/gui")

    clean = subparsers.add_parser("clean", help="清理工作区运行目录")
    clean.add_argument("--runs", default="runs")
    clean.add_argument("--all", action="store_true", help="同时清理 samples")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "samples":
            manifest = generate_samples(args.out, args.sample_ids)
            if args.json:
                print(json.dumps(manifest, indent=2, ensure_ascii=False))
            else:
                print(f"已生成 {len(manifest['samples'])} 个固定种子样本: {args.out}")
                for entry in manifest["samples"]:
                    print(
                        f"  {entry['sample_id']}: "
                        f"{entry['width']}x{entry['height']}x{entry['channels']}"
                    )
            return 0

        if args.command == "run":
            generate_samples(args.samples, [args.sample])
            sample = load_sample(args.samples, args.sample)
            config = PipelineConfig(
                low_threshold=args.low,
                high_threshold=args.high,
                open_size=args.open_size,
                close_size=args.close_size,
                thin_iterations=args.thin,
                prune_length=args.prune,
                tile_size=args.tile,
                max_parallel=args.parallel,
                memory_budget_bytes=args.memory_mb * 1024 * 1024,
                thread_buffer_pixels=args.thread_pixels,
                band_override=args.band,
            )
            fault_once = []
            if args.fault:
                stage_name, block_text = args.fault.split(":", 1)
                from .stages import Stage

                fault_once.append((Stage(stage_name), int(block_text)))
            pipeline = Pipeline(
                sample,
                config,
                run_dir=args.run_dir,
                execution_mode=args.mode,
                fault_once=fault_once,
            )
            pipeline.run(stages_limit=args.stages, retry_failed=args.retry)
            payload = {
                "sample": sample.sample_id,
                "mode": args.mode,
                "tile_size": config.tile_size,
                "blocks": len(pipeline.blocks),
                "progress": pipeline.progress,
                "resumed_commits": pipeline.resumed_commits,
                "snapshot_invalidated": pipeline.snapshot_invalidated,
                "queue_event_count": len(pipeline.queue_events),
                "budget_peak_bytes": pipeline.budget_peak_bytes,
                "metrics": pipeline.stage_metrics,
            }
            print(json.dumps(payload, indent=2, ensure_ascii=False))
            return 0

        if args.command == "verify":
            root = Path(__file__).resolve().parent.parent
            report = run_verification(
                project_root=root,
                samples_dir=args.samples,
                work_dir=args.work_dir,
                report_path=args.json_out,
            )
            _print_verification(report)
            return 0 if report["ok"] else 1

        if args.command == "gui":
            from .gui import run_gui

            run_gui(samples_dir=args.samples, run_dir=args.run_dir)
            return 0

        if args.command == "clean":
            runs = Path(args.runs)
            if runs.exists():
                shutil.rmtree(runs)
                print(f"已清理 {runs}")
            if args.all:
                samples = Path("samples")
                if samples.exists():
                    shutil.rmtree(samples)
                    print(f"已清理 {samples}")
            return 0
    except EdgeForgeError as exc:
        print(json.dumps(exc.as_dict(), indent=2, ensure_ascii=False), file=sys.stderr)
        return 2
    except Exception as exc:
        print(
            json.dumps(
                {
                    "code": "E_INTERNAL",
                    "message": str(exc),
                    "exception": type(exc).__name__,
                },
                indent=2,
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )
        return 3
    parser.print_help()
    return 2


def _print_verification(report: dict[str, Any]) -> None:
    print("EdgeForge 确定性验收")
    print(f"  总耗时: {report['elapsed_seconds']:.3f}s")
    for check in report["checks"]:
        print(f"  [{'PASS' if check['ok'] else 'FAIL'}] {check['name']}")
    print("  异常码:")
    for item in report.get("error_codes", []):
        observed = item["observed"] or {}
        print(
            f"    {item['expected']}: "
            f"{observed.get('code')} field={observed.get('field')} "
            f"block={observed.get('block_id')} stage={observed.get('stage')}"
        )
    print(f"  JSON: {'OK' if report['ok'] else 'FAIL'}")


if __name__ == "__main__":
    raise SystemExit(main())
