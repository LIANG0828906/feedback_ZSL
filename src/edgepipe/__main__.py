"""Command line interface for samples, pipeline runs and acceptance checks."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional

import cv2

from .config import ALGORITHM_VERSION
from .engine import PipelineEngine
from .errors import EdgePipeError, ErrorCode
from .samples import (
    decode_image,
    find_sample,
    generate_samples,
    load_manifest,
    sample_image_path,
    truth_from_sample,
)
from .verify import build_sample_config, verify_all

EXIT_CODES = {
    ErrorCode.PARA: 2,
    ErrorCode.STATE: 3,
    ErrorCode.DIFF: 4,
    ErrorCode.BAND: 5,
    ErrorCode.DECODE: 6,
    ErrorCode.IO: 7,
    ErrorCode.CANCELLED: 8,
    ErrorCode.INTERNAL: 70,
}


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="edgepipe",
        description="确定性分块边缘检测、形态学处理与可恢复检查点工具",
    )
    parser.add_argument("--version", action="version", version=ALGORITHM_VERSION)
    subparsers = parser.add_subparsers(dest="command", required=True)

    samples = subparsers.add_parser("samples", help="生成确定性样本与真值清单")
    samples.add_argument("--dir", default="samples")
    samples.add_argument("--clean", action="store_true")

    run = subparsers.add_parser("run", help="运行一次管线")
    run.add_argument("--manifest", default="samples/index.json")
    run.add_argument("--sample", default=None)
    run.add_argument("--mode", choices=("full", "resume", "incremental"), default="full")
    run.add_argument("--checkpoint-dir", default=None)
    run.add_argument("--report", default=None)
    run.add_argument("--mask", default=None)
    run.add_argument("--config-file", default=None)
    run.add_argument(
        "--set",
        action="append",
        default=[],
        metavar="KEY=JSON_VALUE",
        help="覆盖推荐参数，例如 --set low_threshold=12 --set block_size=64",
    )
    run.add_argument("--stop-after-commits", type=int, default=None)
    run.add_argument("--crash-after-commits", type=int, default=None)

    verify = subparsers.add_parser(
        "verify", help="执行三路径、边界、检查点与异常码验收"
    )
    verify.add_argument("--manifest", default="samples/index.json")
    verify.add_argument("--work-dir", default="artifacts/validation_work")
    verify.add_argument("--report", default="reports/validation_report.json")
    verify.add_argument("--markdown", default="docs/VALIDATION.md")
    verify.add_argument("--regenerate-samples", action="store_true")

    gui = subparsers.add_parser("gui", help="启动 PyQt5 诊断界面")
    gui.add_argument("--manifest", default="samples/index.json")
    gui.add_argument("--checkpoint-root", default="checkpoints/gui")
    return parser


def _parse_override(value: str) -> tuple[str, Any]:
    if "=" not in value:
        raise EdgePipeError(
            ErrorCode.PARA,
            f"--set 需要 KEY=VALUE 格式: {value}",
            field="--set",
        )
    key, raw = value.split("=", 1)
    if not key:
        raise EdgePipeError(ErrorCode.PARA, "参数名不能为空", field="--set")
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError:
        parsed = raw
    return key, parsed


def _overrides(args: argparse.Namespace) -> Dict[str, Any]:
    values: Dict[str, Any] = {}
    if args.config_file:
        path = Path(args.config_file)
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, UnicodeError, json.JSONDecodeError) as exc:
            raise EdgePipeError(
                ErrorCode.PARA,
                f"配置文件不可读: {path}",
                field="--config-file",
                details={"error": str(exc)},
            ) from exc
        if not isinstance(data, dict):
            raise EdgePipeError(
                ErrorCode.PARA,
                "配置文件必须为 JSON 对象",
                field="--config-file",
            )
        values.update(data)
    for item in args.set:
        key, value = _parse_override(item)
        values[key] = value
    return values


def _command_samples(args: argparse.Namespace) -> int:
    manifest = generate_samples(args.dir, clean=args.clean)
    print(
        json.dumps(
            {
                "samples_dir": str(Path(args.dir).resolve()),
                "count": manifest["sample_count"],
                "names": [item["name"] for item in manifest["samples"]],
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0


def _command_run(args: argparse.Namespace) -> int:
    manifest_path = Path(args.manifest)
    if not manifest_path.exists():
        raise EdgePipeError(
            ErrorCode.DECODE,
            f"样本清单不存在: {manifest_path}",
            field="manifest",
        )
    manifest = load_manifest(manifest_path)
    sample = (
        find_sample(manifest, args.sample)
        if args.sample
        else dict(manifest["samples"][0])
    )
    config = build_sample_config(sample, _overrides(args))
    image = decode_image(sample_image_path(sample, manifest_path.parent))
    truth = truth_from_sample(sample)
    checkpoint_dir = Path(
        args.checkpoint_dir or f"checkpoints/cli/{sample['name']}"
    )
    engine = PipelineEngine(config, checkpoint_dir)

    crash_hook = None
    if args.crash_after_commits is not None:
        def crash_hook(_count: int) -> None:
            sys.stdout.flush()
            sys.stderr.flush()
            os._exit(17)

    result = engine.run(
        image,
        truth=truth,
        mode=args.mode,
        stop_after_commits=args.stop_after_commits,
        crash_after_commits=args.crash_after_commits,
        crash_hook=crash_hook,
    )
    mask_path = None
    if result.complete and result.final_mask is not None:
        target = Path(
            args.mask
            or f"reports/masks/{sample['name']}_{args.mode}.png"
        )
        target.parent.mkdir(parents=True, exist_ok=True)
        if not cv2.imwrite(str(target), result.final_mask):
            raise EdgePipeError(
                ErrorCode.IO, f"无法写入掩膜: {target}", field="--mask"
            )
        mask_path = str(target)

    report = {
        "schema": "edgepipe.run-report/v1",
        "sample": sample["name"],
        "image_shape": list(image.shape),
        "result": result.summary(),
        "mask_path": mask_path,
    }
    report_path = Path(
        args.report or f"reports/run_{sample['name']}_{args.mode}.json"
    )
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True),
        encoding="utf-8",
    )
    print(
        json.dumps(
            {
                "sample": sample["name"],
                "mode": args.mode,
                "complete": result.complete,
                "cancelled": result.cancelled,
                "committed": result.progress["committed"],
                "total": result.progress["total"],
                "restored_commits": result.restored_commits,
                "newly_committed": result.newly_committed,
                "invalidated_reason": result.invalidated_reason,
                "geometry_all_pass": (
                    None
                    if result.geometry is None
                    else result.geometry["all_pass"]
                ),
                "mask_path": mask_path,
                "report_path": str(report_path),
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0


def _command_verify(args: argparse.Namespace) -> int:
    report = verify_all(
        manifest_path=args.manifest,
        work_dir=args.work_dir,
        report_path=args.report,
        markdown_path=args.markdown,
        regenerate_samples=args.regenerate_samples,
    )
    summary = {
        "all_pass": report["all_pass"],
        "samples": [
            {
                "name": item["name"],
                "all_pass": item.get("all_pass", False),
                "error": item.get("error"),
            }
            for item in report["samples"]
        ],
        "error_codes": {
            key: value["code"] for key, value in report["error_codes"].items()
        },
        "report": str(Path(args.report)),
        "markdown": str(Path(args.markdown)),
    }
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0 if report["all_pass"] else 1


def _command_gui(args: argparse.Namespace) -> int:
    from .gui import main as gui_main

    return gui_main(
        [sys.argv[0]],
        manifest_path=args.manifest,
        checkpoint_root=args.checkpoint_root,
    )


def main(argv: Optional[List[str]] = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "samples":
            return _command_samples(args)
        if args.command == "run":
            return _command_run(args)
        if args.command == "verify":
            return _command_verify(args)
        if args.command == "gui":
            return _command_gui(args)
        parser.error("未知命令")
    except EdgePipeError as exc:
        print(
            json.dumps(exc.to_dict(), ensure_ascii=False, indent=2),
            file=sys.stderr,
        )
        return EXIT_CODES.get(exc.code, 70)
    return 70


if __name__ == "__main__":
    raise SystemExit(main())
