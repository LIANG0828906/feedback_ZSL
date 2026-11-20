from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from .codec import read_atlas
from .errors import AtlasError, ErrorCode
from .export import export_published, verify_export
from .opengl_preview import check_opengl
from .pipeline import AtlasPipeline
from .render import render_preview_png
from .sample import generate_atlas, generate_suite


def _print_json(value: Any) -> None:
    print(json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False))


def _pipeline_from_args(args: argparse.Namespace) -> AtlasPipeline:
    snapshot = getattr(args, "snapshot", None)
    atlas = getattr(args, "atlas", None)
    if snapshot:
        return AtlasPipeline.from_snapshot(snapshot)
    if atlas:
        return AtlasPipeline(read_atlas(atlas))
    raise AtlasError(
        ErrorCode.E_USAGE,
        "provide --snapshot or --atlas",
        field="input",
    )


def _parse_index_value(value: str, *, option: str) -> tuple[int, str]:
    if "=" not in value:
        raise AtlasError(
            ErrorCode.E_USAGE,
            f"{option} expects TILE=VALUE",
            field=option,
        )
    index, raw = value.split("=", 1)
    try:
        tile = int(index)
    except ValueError:
        raise AtlasError(
            ErrorCode.E_USAGE,
            f"{option} tile index must be an integer",
            field=option,
        ) from None
    return tile, raw


def _command_generate(args: argparse.Namespace) -> int:
    if args.suite:
        records = generate_suite(args.suite, seed=args.seed)
        _print_json({"ok": True, "suite": str(Path(args.suite)), "presets": records})
        return 0
    if not args.output:
        raise AtlasError(
            ErrorCode.E_USAGE,
            "generate requires --output or --suite",
            field="output",
        )
    atlas = generate_atlas(
        args.output,
        width=args.width,
        height=args.height,
        bit_depth=args.bit_depth,
        color_space=args.color_space,
        transfer=args.transfer,
        grid_cols=args.grid_cols,
        grid_rows=args.grid_rows,
        source_peak_nits=args.source_peak,
        seed=args.seed,
    )
    pipeline = AtlasPipeline(atlas)
    baseline = pipeline.current_version_record
    _print_json(
        {
            "ok": True,
            "path": str(args.output),
            "source_hash": atlas.source_hash,
            "header": atlas.header.to_dict(),
            "tiles": [tile.to_dict() for tile in atlas.tiles],
            "baseline_version": baseline.number,
            "baseline_chain_hash": baseline.chain_hash,
            "baseline_content_hash": baseline.content_hash,
        }
    )
    return 0


def _command_inspect(args: argparse.Namespace) -> int:
    atlas = read_atlas(args.atlas)
    _print_json(
        {
            "ok": True,
            "source_hash": atlas.source_hash,
            "payload_hash": atlas.payload_hash,
            "fingerprint": atlas.fingerprint,
            "header": atlas.header.to_dict(),
            "tiles": [tile.to_dict() for tile in atlas.tiles],
        }
    )
    return 0


def _command_convert(args: argparse.Namespace) -> int:
    if args.snapshot:
        pipeline = AtlasPipeline.from_snapshot(args.snapshot)
    else:
        pipeline = AtlasPipeline(read_atlas(args.atlas))
    if args.snapshot_out:
        pipeline.set_checkpoint_path(args.snapshot_out)
    updates: dict[int, dict[str, Any]] = {}
    for item in args.exposure:
        tile, raw = _parse_index_value(item, option="--exposure")
        updates.setdefault(tile, {})["exposure_ev"] = float(raw)
    for item in args.curve:
        tile, raw = _parse_index_value(item, option="--curve")
        updates.setdefault(tile, {})["tone_curve"] = raw
    for item in args.strength:
        tile, raw = _parse_index_value(item, option="--strength")
        updates.setdefault(tile, {})["tone_strength"] = float(raw)
    config: dict[str, Any] = {
        "target_color_space": args.target_color_space,
        "target_transfer": args.target_transfer,
        "target_peak_nits": args.target_peak,
        "source_white_point": args.source_white_point,
        "target_white_point": args.target_white_point,
        "adaptation": args.adaptation,
    }
    version = pipeline.submit(
        config=config,
        tile_updates=updates,
        expected_version=args.expected_version,
    )
    result: dict[str, Any] = {
        "ok": True,
        "version": version.number,
        "parent_version": version.parent_version,
        "chain_hash": version.chain_hash,
        "content_hash": version.content_hash,
        "affected_tiles": list(version.affected_tiles),
        "reused_tiles": list(version.reused_tiles),
    }
    if args.snapshot_out:
        result["snapshot"] = str(args.snapshot_out)
        result["snapshot_sha256"] = pipeline.save_snapshot(args.snapshot_out)
    if args.export_atlas or args.report:
        if not args.export_atlas or not args.report:
            raise AtlasError(
                ErrorCode.E_USAGE,
                "--export-atlas and --report must be used together",
                field="report" if args.export_atlas else "export_atlas",
            )
        export = export_published(
            pipeline,
            args.export_atlas,
            args.report,
            expected_version=version.number,
            expected_content_hash=version.content_hash,
        )
        result["export"] = export.to_dict()
    if args.trace:
        pipeline.write_trace(args.trace)
        result["trace"] = str(args.trace)
    _print_json(result)
    return 0


def _command_recover(args: argparse.Namespace) -> int:
    pipeline = AtlasPipeline.from_snapshot(args.snapshot, resume=True)
    result: dict[str, Any] = {
        "ok": True,
        "recovery": pipeline.recovery_report,
        "current_version": pipeline.current_version_number,
        "chain_hash": pipeline.current_version_record.chain_hash,
    }
    if args.export_dir:
        directory = Path(args.export_dir)
        directory.mkdir(parents=True, exist_ok=True)
        number = pipeline.current_version_number
        atlas_path = directory / f"v{number}.atlas"
        report_path = directory / f"v{number}.report.json"
        trace_path = directory / "trace.json"
        exported = export_published(
            pipeline,
            atlas_path,
            report_path,
            expected_version=number,
        )
        verified = verify_export(atlas_path, report_path)
        pipeline.write_trace(trace_path)
        result.update(
            {
                "export": exported.to_dict(),
                "verification": verified,
                "trace": str(trace_path),
            }
        )
    _print_json(result)
    return 0


def _command_export(args: argparse.Namespace) -> int:
    pipeline = _pipeline_from_args(args)
    number = pipeline.current_version_number
    version = pipeline.current_version_record
    result = export_published(
        pipeline,
        args.export_atlas,
        args.report,
        expected_version=number,
        expected_content_hash=version.content_hash,
    )
    verification = verify_export(args.export_atlas, args.report)
    if args.trace:
        pipeline.write_trace(args.trace)
    _print_json(
        {
            "ok": True,
            "export": result.to_dict(),
            "verification": verification,
        }
    )
    return 0


def _command_verify_export(args: argparse.Namespace) -> int:
    _print_json(
        {
            "ok": True,
            **verify_export(args.export_atlas, args.report),
        }
    )
    return 0


def _command_preview(args: argparse.Namespace) -> int:
    pipeline = _pipeline_from_args(args)
    result = render_preview_png(
        pipeline,
        args.output,
        version=args.version,
        heatmap=args.heatmap,
        scale=args.scale,
        renderer=args.renderer,
    )
    _print_json(
        {
            "ok": True,
            "output": str(args.output),
            "version": (
                pipeline.current_version_number
                if args.version is None
                else int(args.version)
            ),
            "heatmap": bool(args.heatmap),
            "renderer": args.renderer,
            "crc32": f"{result:08x}",
        }
    )
    return 0


def _command_opengl_check(args: argparse.Namespace) -> int:
    info = check_opengl()
    _print_json({"ok": info.available, **info.to_dict()})
    return 0 if info.available else 1


def _command_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    run_gui(
        args.atlas,
        host=args.host,
        port=args.port,
        export_dir=args.export_dir,
        renderer=args.renderer,
        open_browser=not args.no_browser,
    )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="atlas-pipeline",
        description="Deterministic tiled HDR atlas conversion pipeline",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic sample atlases")
    generate.add_argument("--output")
    generate.add_argument("--suite")
    generate.add_argument("--seed", type=int, default=7)
    generate.add_argument("--width", type=int, default=64)
    generate.add_argument("--height", type=int, default=48)
    generate.add_argument("--bit-depth", type=int, default=10)
    generate.add_argument(
        "--color-space",
        choices=["rec709", "display-p3", "rec2020"],
        default="rec709",
    )
    generate.add_argument(
        "--transfer",
        choices=["linear", "srgb", "pq"],
        default="srgb",
    )
    generate.add_argument("--grid-cols", type=int, default=2)
    generate.add_argument("--grid-rows", type=int, default=2)
    generate.add_argument("--source-peak", type=float, default=1000.0)
    generate.set_defaults(handler=_command_generate)

    inspect = subparsers.add_parser("inspect", help="parse and inspect an atlas")
    inspect.add_argument("atlas")
    inspect.set_defaults(handler=_command_inspect)

    convert = subparsers.add_parser("convert", help="submit one atlas conversion")
    convert.add_argument("--atlas")
    convert.add_argument("--snapshot")
    convert.add_argument("--expected-version", type=int)
    convert.add_argument(
        "--target-color-space",
        choices=["rec709", "display-p3", "rec2020"],
        default="rec709",
    )
    convert.add_argument(
        "--target-transfer",
        choices=["linear", "srgb", "pq"],
        default="srgb",
    )
    convert.add_argument("--target-peak", type=float, default=1000.0)
    convert.add_argument(
        "--source-white-point",
        choices=["D65", "D50"],
        default="D65",
    )
    convert.add_argument(
        "--target-white-point",
        choices=["D65", "D50"],
        default="D65",
    )
    convert.add_argument(
        "--adaptation",
        choices=["bradford", "none"],
        default="bradford",
    )
    convert.add_argument("--exposure", action="append", default=[], metavar="TILE=EV")
    convert.add_argument("--curve", action="append", default=[], metavar="TILE=CURVE")
    convert.add_argument("--strength", action="append", default=[], metavar="TILE=VALUE")
    convert.add_argument("--snapshot-out")
    convert.add_argument("--export-atlas")
    convert.add_argument("--report")
    convert.add_argument("--trace")
    convert.set_defaults(handler=_command_convert)

    recover = subparsers.add_parser("recover", help="verify and resume a snapshot")
    recover.add_argument("snapshot")
    recover.add_argument("--export-dir")
    recover.set_defaults(handler=_command_recover)

    export = subparsers.add_parser("export", help="export the latest version and report")
    export.add_argument("--atlas")
    export.add_argument("--snapshot")
    export.add_argument("--export-atlas", required=True)
    export.add_argument("--report", required=True)
    export.add_argument("--trace")
    export.set_defaults(handler=_command_export)

    verify = subparsers.add_parser("verify-export", help="reload an export and verify hashes")
    verify.add_argument("export_atlas")
    verify.add_argument("report")
    verify.set_defaults(handler=_command_verify_export)

    preview = subparsers.add_parser("preview", help="render a deterministic PNG preview")
    preview.add_argument("--atlas")
    preview.add_argument("--snapshot")
    preview.add_argument("--version", type=int)
    preview.add_argument("--output", required=True)
    preview.add_argument("--heatmap", action="store_true")
    preview.add_argument("--scale", type=int, default=4)
    preview.add_argument("--renderer", choices=["cpu", "opengl"], default="cpu")
    preview.set_defaults(handler=_command_preview)

    opengl = subparsers.add_parser("opengl-check", help="create a PyOpenGL EGL context")
    opengl.set_defaults(handler=_command_opengl_check)

    gui = subparsers.add_parser("gui", help="launch the local graphical preview studio")
    gui.add_argument("atlas")
    gui.add_argument("--host", default="127.0.0.1")
    gui.add_argument("--port", type=int, default=8765)
    gui.add_argument("--export-dir", default="atlas-export")
    gui.add_argument("--renderer", choices=["cpu", "opengl", "auto"], default="cpu")
    gui.add_argument("--no-browser", action="store_true")
    gui.set_defaults(handler=_command_gui)

    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    try:
        args = parser.parse_args(argv)
        return int(args.handler(args))
    except AtlasError as error:
        print(
            json.dumps({"ok": False, "error": error.to_dict()}, ensure_ascii=False),
            file=sys.stderr,
        )
        return 2 if error.code == ErrorCode.E_USAGE else 1
    except (OSError, ValueError) as error:
        print(
            json.dumps(
                {
                    "ok": False,
                    "error": {
                        "code": "E_USAGE",
                        "message": str(error),
                    },
                },
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )
        return 2
    except Exception as error:
        print(
            json.dumps(
                {
                    "ok": False,
                    "error": {
                        "code": "E_USAGE",
                        "message": f"{type(error).__name__}: {error}",
                    },
                },
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
