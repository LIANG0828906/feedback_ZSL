"""Command line entry points."""

from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any

from .errors import FogError, error_document
from .pipeline import run_render
from .sample_scene import scene_spec
from .scene import load_scene, parse_scene


def _print_error(error: FogError) -> None:
    print(
        json.dumps(
            error_document(error),
            ensure_ascii=False,
            sort_keys=True,
        ),
        file=sys.stderr,
    )


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="fogvol",
        description="确定性离线舞台雾体积渲染与恢复验证",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    render = subparsers.add_parser("render", help="渲染场景")
    render.add_argument("--scene", required=True, help="JSON 场景文件")
    render.add_argument("--out", required=True, help="输出目录")
    render.add_argument(
        "--fresh",
        action="store_true",
        help="忽略并删除已有快照，从零开始",
    )
    render.add_argument(
        "--interrupt-after-chunks",
        type=int,
        default=None,
        metavar="N",
        help="本次完成 N 个新块后正常中断，用于恢复验证",
    )

    make = subparsers.add_parser("make-scene", help="生成本地确定性场景")
    make.add_argument("--out", required=True, help="JSON 输出路径")
    make.add_argument(
        "--embed-density",
        action="store_true",
        help="把确定性生成的雾体素也写入 JSON",
    )

    validate = subparsers.add_parser("validate", help="仅校验场景")
    validate.add_argument("--scene", required=True)
    return parser


def _cmd_render(args: argparse.Namespace) -> int:
    if (
        args.interrupt_after_chunks is not None
        and args.interrupt_after_chunks < 1
    ):
        raise FogError(
            "INVALID_ARGUMENT",
            "--interrupt-after-chunks",
            "必须为正整数",
        )
    scene = load_scene(args.scene)
    stop_requested = False

    def handle_signal(_signum: int, _frame: Any) -> None:
        nonlocal stop_requested
        stop_requested = True

    previous = signal.signal(signal.SIGINT, handle_signal)
    try:
        result = run_render(
            scene,
            args.out,
            fresh=args.fresh,
            interrupt_after_chunks=args.interrupt_after_chunks,
            should_stop=lambda: stop_requested,
        )
    finally:
        signal.signal(signal.SIGINT, previous)

    if result.status == "interrupted":
        print(
            json.dumps(
                {
                    "ok": True,
                    "status": "interrupted",
                    "recovery_report": str(
                        result.recovery_report_path.relative_to(result.output_dir)
                    )
                    if result.recovery_report_path
                    else None,
                },
                ensure_ascii=False,
                sort_keys=True,
            )
        )
        return 0
    if result.status in {"resume_mismatch", "quality_failed"}:
        print(
            json.dumps(
                {
                    "ok": False,
                    "status": result.status,
                    "error": result.error,
                },
                ensure_ascii=False,
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 3
    print(
        json.dumps(
            {
                "ok": True,
                "status": "complete",
                "report": str(result.report_path.relative_to(result.output_dir))
                if result.report_path
                else None,
            },
            ensure_ascii=False,
            sort_keys=True,
        )
    )
    return 0


def _cmd_make_scene(args: argparse.Namespace) -> int:
    spec = scene_spec(embedded_density=args.embed_density)
    parse_scene(spec)
    path = Path(args.out)
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(
        spec,
        ensure_ascii=False,
        allow_nan=False,
        indent=2,
        sort_keys=True,
    )
    path.write_text(text + "\n", encoding="utf-8")
    print(
        json.dumps(
            {
                "ok": True,
                "scene": str(path),
                "embedded_density": bool(args.embed_density),
            },
            ensure_ascii=False,
            sort_keys=True,
        )
    )
    return 0


def _cmd_validate(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    print(
        json.dumps(
            {
                "ok": True,
                "scene_id": scene.scene_id,
                "scene_digest": scene.digest,
                "voxel_count": int(scene.volume.density.size),
                "lights": len(scene.lights),
            },
            ensure_ascii=False,
            sort_keys=True,
        )
    )
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "render":
            return _cmd_render(args)
        if args.command == "make-scene":
            return _cmd_make_scene(args)
        if args.command == "validate":
            return _cmd_validate(args)
    except FogError as error:
        _print_error(error)
        return 2
    except OSError as exc:
        _print_error(
            FogError("IO_ERROR", "$", f"文件操作失败: {exc.strerror}")
        )
        return 2
    except Exception as exc:
        _print_error(
            FogError(
                "INTERNAL_ERROR",
                "$",
                f"{type(exc).__name__}: {exc}",
            )
        )
        return 2
    parser.error("unknown command")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())

