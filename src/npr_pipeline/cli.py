from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from .canonical import canonical_json_bytes, canonical_json_text
from .errors import (
    E_COMMIT_FAILED,
    E_INVALID_REQUEST,
    E_RECOVERY_INVALID,
    E_SCENE_INVALID,
    PipelineError,
    fail,
)
from .params import RenderParams
from .pipeline import RenderPipeline
from .preview import PreviewService
from .recovery import recover
from .samples import generate_samples
from .scene import load_scene
from .storage import Store
from .trace import TraceWriter, result_trace_records, write_error_trace


def _load_params(path: str | Path) -> RenderParams:
    try:
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
    except OSError as exc:
        fail(E_INVALID_REQUEST, "params_file", "read_failed", reason=str(exc))
    except json.JSONDecodeError as exc:
        fail(
            E_INVALID_REQUEST,
            "params_file",
            "invalid_json",
            line=exc.lineno,
            column=exc.colno,
        )
    return RenderParams.from_dict(raw)


def _result_summary(result: Any, preview: Any | None = None) -> dict[str, Any]:
    return {
        "version": result.version,
        "content_hash": result.version_hash,
        "manifest_hash": result.manifest["manifest_hash"],
        "committed": result.committed,
        "affected_batches": result.affected_batches,
        "reused_batches": result.reused_batches,
        "rewritten_batches": result.rewritten_batches,
        "resumed_batches": result.resumed_batches,
        "removed_batches": result.removed_batches,
        "layer_stats": result.layer_stats,
        "preview_hash": None if preview is None else preview.content_hash,
    }


def _cmd_generate(args: argparse.Namespace) -> dict[str, Any]:
    manifest = generate_samples(
        args.out,
        seed=args.seed,
        render_baselines=not args.no_baselines,
    )
    return {
        "out": str(Path(args.out)),
        "seed": args.seed,
        "cases": [
            {
                "name": case["name"],
                "version_hash": case["version_hash"],
                "scene": case["scene"],
                "params": case["params"],
            }
            for case in manifest["cases"]
        ],
    }


def _cmd_parse_scene(args: argparse.Namespace) -> dict[str, Any]:
    scene = load_scene(args.scene)
    document = scene.geometry_document()
    if args.out:
        output = Path(args.out)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(canonical_json_bytes(document) + b"\n")
    return {
        "scene_id": scene.scene_id,
        "path_count": len(scene.paths),
        "zone_count": len(scene.zones),
        "draw_order": list(scene.draw_order),
        "geometry": None if args.out else document,
    }


def _cmd_render(args: argparse.Namespace) -> dict[str, Any]:
    scene = load_scene(args.scene)
    params = _load_params(args.params)
    store = Store(args.workdir)
    pipeline = RenderPipeline(store)
    try:
        result = pipeline.render(
            scene,
            params,
            resume=not args.no_resume,
            crash_after_batches=args.crash_after_batch,
            fail_after_batches=args.fail_after_batch,
        )
    except PipelineError as exc:
        if args.trace:
            write_error_trace(
                args.trace,
                operation=args.operation,
                code=exc.code,
                location=exc.location,
                detail=exc.detail,
            )
        raise
    if args.trace:
        with TraceWriter(args.trace) as writer:
            writer.write(
                result_trace_records(result, operation=args.operation)
            )
    preview = PreviewService(store).refresh()
    return _result_summary(result, preview)


def _cmd_recover(args: argparse.Namespace) -> dict[str, Any]:
    store = Store(args.workdir)
    result = recover(
        store,
        cleanup=not args.no_cleanup,
        keep_staging=not args.discard_staging,
    )
    payload = result.to_dict()
    if args.trace:
        with TraceWriter(args.trace) as writer:
            writer.write(
                [
                    {
                        "type": "recovery",
                        "version": payload["version"],
                        "content_hash": payload["content_hash"],
                        "resume_available": payload["resume_available"],
                        "completed_batches": len(payload["completed_batches"]),
                        "missing_batches": len(payload["missing_batches"]),
                        "discarded_chunks": len(payload["discarded_chunks"]),
                        "discarded_temp_files": len(payload["discarded_temp_files"]),
                    }
                ]
            )
    return payload


def _cmd_regression(args: argparse.Namespace) -> dict[str, Any]:
    from .regression import run_regression

    return run_regression(args.out, seed=args.seed)


def _cmd_ui(args: argparse.Namespace) -> dict[str, Any]:
    from .ui import run_ui

    scene = load_scene(args.scene)
    params = _load_params(args.params)
    Store(args.workdir).initialize()
    return run_ui(
        scene=scene,
        params=params,
        store=Store(args.workdir),
        refresh_ms=args.refresh_ms,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="npr-pipeline")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=20261122)
    generate.add_argument("--no-baselines", action="store_true")
    generate.set_defaults(handler=_cmd_generate)

    parse_scene = subparsers.add_parser("parse-scene", help="validate and normalize a scene")
    parse_scene.add_argument("--scene", required=True)
    parse_scene.add_argument("--out")
    parse_scene.set_defaults(handler=_cmd_parse_scene)

    render = subparsers.add_parser("render", help="render or incrementally update")
    render.add_argument("--scene", required=True)
    render.add_argument("--params", required=True)
    render.add_argument("--workdir", required=True)
    render.add_argument("--trace")
    render.add_argument("--operation", default="render")
    render.add_argument("--no-resume", action="store_true")
    render.add_argument("--crash-after-batch", type=int)
    render.add_argument("--fail-after-batch", type=int)
    render.set_defaults(handler=_cmd_render)

    recover_cmd = subparsers.add_parser("recover", help="verify and recover committed state")
    recover_cmd.add_argument("--workdir", required=True)
    recover_cmd.add_argument("--trace")
    recover_cmd.add_argument("--no-cleanup", action="store_true")
    recover_cmd.add_argument("--discard-staging", action="store_true")
    recover_cmd.set_defaults(handler=_cmd_recover)

    regression = subparsers.add_parser("regression", help="run deterministic regression suite")
    regression.add_argument("--out", required=True)
    regression.add_argument("--seed", type=int, default=20261122)
    regression.set_defaults(handler=_cmd_regression)

    ui = subparsers.add_parser("ui", help="open the fixed-interval preview window")
    ui.add_argument("--scene", required=True)
    ui.add_argument("--params", required=True)
    ui.add_argument("--workdir", required=True)
    ui.add_argument("--refresh-ms", type=int, default=200)
    ui.set_defaults(handler=_cmd_ui)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        result = args.handler(args)
    except PipelineError as exc:
        print(canonical_json_text(exc.to_dict()), file=sys.stderr)
        return {
            E_SCENE_INVALID: 2,
            E_COMMIT_FAILED: 3,
            E_RECOVERY_INVALID: 4,
            E_INVALID_REQUEST: 5,
        }.get(exc.code, 1)
    print(canonical_json_text(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
