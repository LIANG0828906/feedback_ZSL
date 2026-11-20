"""Command-line entry points for rendering, samples, recovery, and acceptance."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Any

import numpy as np

from .chunks import build_chunk_table
from .config import RenderConfig
from .errors import DustRenderError
from .incremental import IncrementalRenderer
from .renderer import ChunkResult
from .samplegen import generate_sample_set
from .scene import Scene
from .scheduler import RenderScheduler
from .snapshot import load_snapshot, save_snapshot
from .util import (
    atomic_write_json,
    canonical_json,
    sha256_file,
)


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except DustRenderError as exc:
        print(canonical_json(exc.to_dict()), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        return 130


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="dustrender",
        description="Deterministic offline volumetric dust/glass preview renderer",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    validate = sub.add_parser("validate", help="parse and validate a DUSTVOL1 scene")
    validate.add_argument("scene")
    validate.set_defaults(func=_cmd_validate)

    samples = sub.add_parser("samples", help="generate deterministic local sample scenes")
    samples.add_argument("--out", required=True)
    samples.add_argument("--seed", type=int, default=20261121)
    samples.add_argument("--no-baselines", action="store_true")
    samples.set_defaults(func=_cmd_samples)

    render = sub.add_parser("render", help="render a whole frame by chunks")
    _add_render_options(render)
    render.add_argument("scene")
    render.add_argument("--out", required=True)
    render.set_defaults(func=_cmd_render)

    resume = sub.add_parser("resume", help="resume a render from an atomic snapshot")
    _add_render_options(resume)
    resume.add_argument("scene")
    resume.add_argument("--snapshot", required=True)
    resume.add_argument("--out", required=True)
    resume.set_defaults(func=_cmd_resume)

    incremental = sub.add_parser(
        "incremental", help="render a changed scene by invalidating only affected chunks"
    )
    _add_render_options(incremental)
    incremental.add_argument("before")
    incremental.add_argument("after")
    incremental.add_argument("--out", required=True)
    incremental.add_argument("--full-reference", action="store_true")
    incremental.set_defaults(func=_cmd_incremental)

    serve = sub.add_parser("serve", help="start a loopback-only local render service")
    serve.add_argument("--workdir", required=True)
    serve.add_argument("--host", default="127.0.0.1")
    serve.add_argument("--port", type=int, default=0)
    serve.set_defaults(func=_cmd_serve)

    gui = sub.add_parser("gui", help="open the local interactive preview")
    _add_render_options(gui)
    gui.add_argument("scene")
    gui.set_defaults(func=_cmd_gui)

    acceptance = sub.add_parser(
        "acceptance", help="run the deterministic local acceptance suite"
    )
    acceptance.add_argument("--out", required=True)
    acceptance.add_argument("--seed", type=int, default=20261121)
    acceptance.add_argument("--reuse-samples", action="store_true")
    acceptance.set_defaults(func=_cmd_acceptance)
    return parser


def _add_render_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--config", help="JSON render configuration")
    parser.add_argument("--chunk-size", help="chunk WIDTHxHEIGHT")
    parser.add_argument("--parallelism", type=int)
    parser.add_argument("--budget", type=int)
    parser.add_argument("--policy", choices=["degrade", "reject"])
    parser.add_argument("--precision", choices=["float", "fixed"])
    parser.add_argument("--compare-precision", action="store_true", default=None)
    parser.add_argument("--seam", action="store_true", default=None)
    parser.add_argument("--no-seam", dest="seam", action="store_false")
    parser.add_argument("--snapshot-each-chunk", action="store_true")


def _config_from_args(args: argparse.Namespace) -> RenderConfig:
    if getattr(args, "config", None):
        config = RenderConfig.load(args.config)
    else:
        config = RenderConfig()
    overrides: dict[str, Any] = {}
    if getattr(args, "chunk_size", None):
        try:
            width, height = args.chunk_size.lower().split("x", 1)
            overrides["chunk_width"] = int(width)
            overrides["chunk_height"] = int(height)
        except ValueError as exc:
            raise DustRenderError(
                "E_CONFIG_INVALID", "chunk size must be WIDTHxHEIGHT", "$.chunk_size"
            ) from exc
    for source, target in (
        ("parallelism", "parallelism"),
        ("budget", "task_budget"),
        ("policy", "budget_policy"),
        ("precision", "precision"),
        ("compare_precision", "compare_precision"),
        ("seam", "seam_validation"),
    ):
        value = getattr(args, source, None)
        if value is not None:
            overrides[target] = value
    return RenderConfig.from_dict({**config.to_dict(), **overrides})


def _cmd_validate(args: argparse.Namespace) -> int:
    scene = Scene.load(args.scene)
    print(
        canonical_json(
            {
                "ok": True,
                "scene_id": scene.scene_id,
                "screen": [scene.width, scene.height],
                "media": len(scene.media),
                "density_version": scene.density_version,
                "source_sha256": sha256_file(args.scene),
                "payload_sha256": scene.payload_sha256,
            }
        )
    )
    return 0


def _cmd_samples(args: argparse.Namespace) -> int:
    manifest = generate_sample_set(
        args.out,
        args.seed,
        render_baselines=not args.no_baselines,
    )
    print(canonical_json(manifest))
    return 0


def _cmd_render(args: argparse.Namespace) -> int:
    scene = Scene.load(args.scene)
    config = _config_from_args(args)
    outdir = Path(args.out)
    snapshot_path = outdir / "render-snapshot.json"

    def progress(
        results: dict[str, ChunkResult],
        statuses: dict[str, str],
        step: float,
    ) -> None:
        if args.snapshot_each_chunk:
            chunks = build_chunk_table(
                scene.width, scene.height, config.chunk_width, config.chunk_height
            )
            save_snapshot(
                snapshot_path,
                scene,
                config,
                chunks,
                results,
                statuses,
                effective_step=step,
            )

    run = RenderScheduler().render(scene, config, on_progress=progress)
    run.write(outdir)
    if args.snapshot_each_chunk:
        save_snapshot(
            snapshot_path,
            scene,
            config,
            run.chunks,
            run.results,
            run.statuses,
            effective_step=run.effective_step,
        )
    print(canonical_json(run.result_document()))
    return 0 if run.error is None else 2


def _cmd_resume(args: argparse.Namespace) -> int:
    scene = Scene.load(args.scene)
    config = _config_from_args(args)
    snapshot_source = Path(args.snapshot)
    if snapshot_source.is_dir():
        from .snapshot import find_latest_valid_snapshot

        snapshot = find_latest_valid_snapshot(snapshot_source, scene, config)
    else:
        snapshot = load_snapshot(snapshot_source, scene, config)
    snapshot_path = snapshot.path
    outdir = Path(args.out)

    def progress(
        results: dict[str, ChunkResult],
        statuses: dict[str, str],
        step: float,
    ) -> None:
        chunks = build_chunk_table(
            scene.width, scene.height, config.chunk_width, config.chunk_height
        )
        save_snapshot(
            snapshot_path,
            scene,
            config,
            chunks,
            results,
            statuses,
            effective_step=step,
        )

    run = RenderScheduler().render(
        scene,
        config,
        preloaded=snapshot.results,
        preloaded_status="recovered",
        on_progress=progress if args.snapshot_each_chunk else None,
    )
    run.write(outdir)
    save_snapshot(
        snapshot_path,
        scene,
        config,
        run.chunks,
        run.results,
        run.statuses,
        effective_step=run.effective_step,
    )
    print(canonical_json(run.result_document()))
    return 0 if run.error is None else 2


def _cmd_incremental(args: argparse.Namespace) -> int:
    before = Scene.load(args.before)
    after = Scene.load(args.after)
    config = _config_from_args(args)
    renderer = IncrementalRenderer()
    base = renderer.render_initial(before, config)
    if base.error is not None:
        base.write(Path(args.out) / "base")
        raise base.error
    report = renderer.render_update(base, after, config)
    outdir = Path(args.out)
    report.run.write(outdir)
    report.run.write(outdir / "base")
    atomic_write_json(outdir / "incremental.json", report.to_dict())
    if args.full_reference:
        reference = RenderScheduler().render(after, config)
        reference.write(outdir / "full-reference")
        if reference.error is not None or not np.array_equal(
            reference.image, report.run.image
        ):
            raise DustRenderError(
                "E_INCREMENTAL_MISMATCH",
                "incremental image differs from the full reference",
                "$incremental",
            )
    print(canonical_json(report.to_dict()))
    return 0 if report.run.error is None else 2


def _cmd_serve(args: argparse.Namespace) -> int:
    from .server import serve

    serve(args.workdir, args.host, args.port)
    return 0


def _cmd_gui(args: argparse.Namespace) -> int:
    from .gui import launch_gui

    launch_gui(args.scene, _config_from_args(args))
    return 0


def _cmd_acceptance(args: argparse.Namespace) -> int:
    from .acceptance import run_acceptance

    report = run_acceptance(Path(args.out), seed=args.seed, reuse_samples=args.reuse_samples)
    print(canonical_json(report))
    return 0 if report.get("ok") else 1
