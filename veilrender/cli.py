"""Command-line entry points."""

from __future__ import annotations

import argparse
import json
import signal
import sys
import time
import traceback
from pathlib import Path
from typing import Any

from .compare import compare_directories
from .errors import RenderConsistencyError, SceneValidationError
from .renderer import RenderContext, render_tile
from .reporting import finalize_artifacts, write_json
from .sample_scene import make_invalid_scene, make_sample_scene, write_scene
from .scene import canonical_json, load_scene, scene_digest
from .snapshot import SnapshotError, SnapshotStore
from .texture import generate_weave_texture, texture_digest

EXIT_OK = 0
EXIT_VALIDATION = 2
EXIT_CONSISTENCY = 3
EXIT_RUNTIME = 4

_FINAL_ARTIFACTS = (
    "rgba.png",
    "layer_ownership.png",
    "layer_ownership.json",
    "projection_landings.json",
    "report.json",
    "run_meta.json",
    "run_metrics.json",
    "recovery.json",
    "differences.json",
    "error.json",
)


def _print_json(value: Any, *, stream: Any = sys.stdout) -> None:
    stream.write(canonical_json(value) + "\n")
    stream.flush()


def _remove_final_artifacts(output_dir: Path) -> None:
    for name in _FINAL_ARTIFACTS:
        path = output_dir / name
        if path.exists():
            path.unlink()


def _structured_runtime_error(code: str, message: str, **fields: Any) -> dict[str, Any]:
    result: dict[str, Any] = {"ok": False, "code": code, "message": message}
    result.update(fields)
    return result


def _write_error_if_possible(output_dir: Path | None, error: dict[str, Any]) -> None:
    if output_dir is None:
        return
    try:
        output_dir.mkdir(parents=True, exist_ok=True)
        write_json(output_dir / "error.json", error)
    except OSError:
        pass


def _parse_resolution(value: str) -> list[int]:
    try:
        first, second = value.lower().split("x", 1)
        width, height = int(first), int(second)
    except (ValueError, AttributeError) as exc:
        raise argparse.ArgumentTypeError("resolution must be WIDTHxHEIGHT") from exc
    if not (1 <= width <= 4096 and 1 <= height <= 4096):
        raise argparse.ArgumentTypeError("resolution values must be in [1,4096]")
    return [width, height]


def _command_generate_scene(args: argparse.Namespace) -> int:
    scene = make_sample_scene(
        width=args.width,
        height=args.height,
        tile_width=args.tile_width,
        tile_height=args.tile_height,
        camera_samples=args.camera_samples,
    )
    write_scene(scene, args.output)
    _print_json(
        {
            "ok": True,
            "scene": str(Path(args.output)),
            "scene_digest": scene_digest(scene),
            "layers": len(scene["layers"]),
            "lights": len(scene["lights"]),
            "image": {
                "width": scene["image"]["width"],
                "height": scene["image"]["height"],
                "tile_width": scene["image"]["tile_width"],
                "tile_height": scene["image"]["tile_height"],
            },
        }
    )
    return EXIT_OK


def _command_make_invalid(args: argparse.Namespace) -> int:
    scene = make_invalid_scene(args.case)
    write_scene(scene, args.output)
    _print_json({"ok": True, "case": args.case, "scene": str(Path(args.output))})
    return EXIT_OK


def _command_validate(args: argparse.Namespace) -> int:
    try:
        scene, digest = load_scene(args.scene)
    except SceneValidationError as exc:
        _print_json(exc.as_dict(), stream=sys.stderr)
        return EXIT_VALIDATION
    _print_json(
        {
            "ok": True,
            "scene_digest": digest,
            "layers": [
                {"layer": layer["layer"], "id": layer["id"], "kind": layer["kind"]}
                for layer in scene["layers"]
            ],
            "lights": [light["id"] for light in scene["lights"]],
        }
    )
    return EXIT_OK


def _command_texture(args: argparse.Namespace) -> int:
    texture = generate_weave_texture(args.seed, args.resolution)
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_bytes(texture.tobytes(order="C"))
    _print_json(
        {
            "ok": True,
            "seed": args.seed,
            "resolution": args.resolution,
            "sha256": texture_digest(texture),
            "output": str(output),
        }
    )
    return EXIT_OK


def _render_snapshot_command(args: argparse.Namespace) -> int:
    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    try:
        scene, scene_content_digest = load_scene(args.scene)
    except SceneValidationError as exc:
        error = exc.as_dict()
        _write_error_if_possible(output_dir, error)
        _print_json(error, stream=sys.stderr)
        return EXIT_VALIDATION

    context = RenderContext.create(scene)
    store = SnapshotStore(
        output_dir,
        scene_digest=scene_content_digest,
        scene_layer_count=len(scene["layers"]),
        image_width=int(scene["image"]["width"]),
        image_height=int(scene["image"]["height"]),
        tile_width=int(scene["image"]["tile_width"]),
        tile_height=int(scene["image"]["tile_height"]),
    )
    _remove_final_artifacts(output_dir)
    mode, invalidation_reason = store.load()
    if args.no_resume:
        store.discard()
        mode = "reset"
        invalidation_reason = "--no-resume requested"
    elif mode == "reset":
        store = SnapshotStore(
            output_dir,
            scene_digest=scene_content_digest,
            scene_layer_count=len(scene["layers"]),
            image_width=int(scene["image"]["width"]),
            image_height=int(scene["image"]["height"]),
            tile_width=int(scene["image"]["tile_width"]),
            tile_height=int(scene["image"]["tile_height"]),
        )
        store.load()

    interrupted = False

    def handle_sigint(_signum: int, _frame: Any) -> None:
        nonlocal interrupted
        interrupted = True

    previous_handler = signal.signal(signal.SIGINT, handle_sigint)
    session_started = time.perf_counter()
    committed_this_run = 0
    pending = store.pending_ids()
    try:
        for tile_id in pending:
            x0, y0 = store.tile_origin(tile_id)
            tile_height, tile_width = store.tile_shape(tile_id)
            rgba, ownership, stats = render_tile(
                scene,
                context,
                x0,
                y0,
                tile_width,
                tile_height,
            )
            store.commit(tile_id, rgba, ownership, stats)
            committed_this_run += 1
            if not args.quiet:
                _print_json(
                    {
                        "event": "tile_committed",
                        "tile_id": tile_id,
                        "completed": len(store.entries),
                        "total": store.total_tiles,
                    },
                    stream=sys.stderr,
                )
            if interrupted:
                break

        remaining = store.pending_ids()
        if interrupted and remaining:
            recovery = {
                "schema_version": "veil-recovery/1",
                "scene_digest": scene_content_digest,
                "interrupted": True,
                "signal": "SIGINT",
                "completed_tile_ids": store.completed_ids(),
                "completed_tile_count": len(store.entries),
                "total_tiles": store.total_tiles,
                "next_pending_tile": remaining[0],
                "snapshot_commit_count": len(store.entries),
                "exit_code": 0,
            }
            write_json(output_dir / "recovery.json", recovery)
            elapsed = time.perf_counter() - session_started
            _print_json(
                {
                    "ok": True,
                    "interrupted": True,
                    "resume_ready": True,
                    "completed_tile_count": len(store.entries),
                    "total_tiles": store.total_tiles,
                    "elapsed_seconds": round(elapsed, 6),
                }
            )
            return EXIT_OK

        if store.pending_ids():
            raise SnapshotError("rendering stopped with uncommitted tiles")

        report = finalize_artifacts(
            output_dir,
            scene,
            scene_content_digest,
            context.geometries,
            store,
        )
        if interrupted:
            recovery = {
                "schema_version": "veil-recovery/1",
                "scene_digest": scene_content_digest,
                "interrupted": True,
                "signal": "SIGINT",
                "completed_tile_ids": store.completed_ids(),
                "completed_tile_count": len(store.entries),
                "total_tiles": store.total_tiles,
                "next_pending_tile": None,
                "snapshot_commit_count": len(store.entries),
                "exit_code": 0,
            }
            write_json(output_dir / "recovery.json", recovery)

        elapsed = time.perf_counter() - session_started
        run_metrics = {
            "schema_version": "veil-run-metrics/1",
            "elapsed_seconds": round(elapsed, 6),
            "snapshot_load_mode": mode,
            "snapshot_invalidation_reason": invalidation_reason,
            "tiles_committed_this_run": committed_this_run,
        }
        write_json(output_dir / "run_metrics.json", run_metrics)

        if args.baseline:
            try:
                compare_directories(args.baseline, output_dir)
            except RenderConsistencyError as exc:
                _print_json(exc.differences, stream=sys.stderr)
                return EXIT_CONSISTENCY

        summary = {
            "ok": True,
            "interrupted": interrupted,
            "resume_ready": True,
            "scene_digest": scene_content_digest,
            "completed_tile_count": len(store.entries),
            "total_tiles": store.total_tiles,
            "snapshot_commit_count": report["snapshot"]["commit_count"],
            "rgba_sha256": report["image"]["rgba_sha256"],
            "layer_ownership_sha256": report["image"]["layer_ownership_sha256"],
            "output_dir": str(output_dir),
            "elapsed_seconds": round(elapsed, 6),
        }
        _print_json(summary)
        return EXIT_OK
    except SnapshotError as exc:
        error = _structured_runtime_error("E_SNAPSHOT", str(exc))
        _write_error_if_possible(output_dir, error)
        _print_json(error, stream=sys.stderr)
        return EXIT_RUNTIME
    except OSError as exc:
        error = _structured_runtime_error("E_IO", str(exc))
        _write_error_if_possible(output_dir, error)
        _print_json(error, stream=sys.stderr)
        return EXIT_RUNTIME
    except Exception as exc:
        error = _structured_runtime_error(
            "E_RUNTIME",
            str(exc),
            exception=type(exc).__name__,
        )
        if args.debug:
            error["traceback"] = traceback.format_exc()
        _write_error_if_possible(output_dir, error)
        _print_json(error, stream=sys.stderr)
        return EXIT_RUNTIME
    finally:
        signal.signal(signal.SIGINT, previous_handler)


def _command_render(args: argparse.Namespace) -> int:
    return _render_snapshot_command(args)


def _command_compare(args: argparse.Namespace) -> int:
    try:
        result = compare_directories(
            args.expected,
            args.actual,
            output_path=args.output,
        )
    except RenderConsistencyError as exc:
        _print_json(exc.differences, stream=sys.stderr)
        return EXIT_CONSISTENCY
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        error = _structured_runtime_error("E_COMPARE_IO", str(exc))
        _print_json(error, stream=sys.stderr)
        return EXIT_RUNTIME
    _print_json(
        {
            "ok": True,
            "difference_count": result["difference_count"],
            "output": (
                str(args.output or (Path(args.actual) / "differences.json"))
                if result["difference_count"]
                else None
            ),
        }
    )
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="veil-render",
        description="Deterministic layered gauze/lattice consistency renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate-scene", help="write the deterministic demo scene")
    generate.add_argument("--output", required=True)
    generate.add_argument("--width", type=int, default=256)
    generate.add_argument("--height", type=int, default=160)
    generate.add_argument("--tile-width", type=int, default=64)
    generate.add_argument("--tile-height", type=int, default=40)
    generate.add_argument("--camera-samples", type=int, default=2)
    generate.set_defaults(handler=_command_generate_scene)

    invalid = subparsers.add_parser("make-invalid", help="write an invalid validation fixture")
    invalid.add_argument(
        "--case",
        required=True,
        choices=[
            "mesh_index",
            "degenerate",
            "transmittance",
            "reflectance",
            "duplicate_layer",
            "duplicate_id",
            "light_inside",
            "camera",
        ],
    )
    invalid.add_argument("--output", required=True)
    invalid.set_defaults(handler=_command_make_invalid)

    validate = subparsers.add_parser("validate", help="validate a scene without rendering")
    validate.add_argument("--scene", required=True)
    validate.set_defaults(handler=_command_validate)

    texture = subparsers.add_parser("texture", help="write a raw deterministic weave table")
    texture.add_argument("--seed", type=int, required=True)
    texture.add_argument("--resolution", type=_parse_resolution, required=True)
    texture.add_argument("--output", required=True)
    texture.set_defaults(handler=_command_texture)

    render = subparsers.add_parser("render", help="render or resume tiled output")
    render.add_argument("--scene", required=True)
    render.add_argument("--output-dir", required=True)
    render.add_argument("--no-resume", action="store_true")
    render.add_argument("--baseline", help="compare final output with this reference directory")
    render.add_argument("--quiet", action="store_true")
    render.add_argument("--debug", action="store_true")
    render.set_defaults(handler=_command_render)

    compare = subparsers.add_parser("compare", help="compare two output directories")
    compare.add_argument("--expected", required=True)
    compare.add_argument("--actual", required=True)
    compare.add_argument("--output")
    compare.set_defaults(handler=_command_compare)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.handler(args))

