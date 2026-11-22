from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from .compare import compare_snapshots
from .errors import PipelineError
from .generator import generate_project
from .model import BakeParams
from .pipeline import Pipeline, inspect_cache


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except PipelineError as exc:
        _print_json({"ok": False, "error": exc.to_dict()})
        return 2
    except KeyboardInterrupt:
        _print_json({"ok": False, "error": {"code": "E_CANCEL", "message": "cancelled by user"}})
        return 130


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="terrain-pipeline",
        description="Deterministic open-world terrain render-data bake pipeline",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate a deterministic local project")
    generate.add_argument("--out", default="artifacts/sample")
    generate.add_argument("--seed", type=int, default=20261123)
    generate.add_argument("--grid", type=int, nargs=2, default=(4, 3), metavar=("X", "Y"))
    generate.add_argument("--tile-size", type=int, default=16)
    generate.add_argument("--subdivisions", type=int, default=4)
    generate.add_argument("--points-per-tile", type=int, default=96)
    generate.set_defaults(handler=_cmd_generate)

    bake = subparsers.add_parser("bake", help="bake, incrementally update, or fully rebuild")
    _add_project_args(bake)
    bake.add_argument("--mode", choices=("auto", "incremental", "full"), default="auto")
    bake.add_argument("--fail-tile", action="append", default=[])
    bake.set_defaults(handler=_cmd_bake)

    edit = subparsers.add_parser("edit", help="persist an edit and run an incremental bake")
    _add_project_args(edit)
    edit.add_argument("--tile", required=True)
    group = edit.add_mutually_exclusive_group(required=True)
    group.add_argument("--height", nargs=3, metavar=("X", "Y", "VALUE"))
    group.add_argument("--material", nargs=2, metavar=("LAYER", "VALUE"))
    edit.add_argument("--no-bake", action="store_true")
    edit.set_defaults(handler=_cmd_edit)

    compare = subparsers.add_parser("compare", help="compare two cache snapshots byte-for-byte")
    compare.add_argument("left_cache")
    compare.add_argument("right_cache")
    compare.add_argument("--no-raise", action="store_true")
    compare.set_defaults(handler=_cmd_compare)

    inspect_parser = subparsers.add_parser("inspect", help="inspect a completed snapshot")
    inspect_parser.add_argument("cache")
    inspect_parser.set_defaults(handler=_cmd_inspect)

    gui = subparsers.add_parser("gui", help="open the Windows preview and diagnostics panel")
    _add_project_args(gui)
    gui.add_argument("--smoke-test", action="store_true", help=argparse.SUPPRESS)
    gui.set_defaults(handler=_cmd_gui)
    return parser


def _add_project_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("project")
    parser.add_argument("--cache", default="artifacts/runtime/cache")
    parser.add_argument("--edits", default=None)
    parser.add_argument("--subdivisions", type=int, default=None)
    parser.add_argument("--normal-strength", type=float, default=1.0)
    parser.add_argument("--min-layer-weight", type=float, default=0.12)
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--max-vertices-per-thread", type=int, default=262_144)


def _params(args: argparse.Namespace) -> BakeParams:
    subdivisions = args.subdivisions
    if subdivisions is None:
        try:
            manifest = json.loads(
                (Path(args.project) / "manifest.json").read_text(encoding="utf-8")
            )
            subdivisions = int(manifest.get("subdivisions", 4))
        except Exception:
            subdivisions = 4
    return BakeParams(
        subdivisions=subdivisions,
        normal_strength=args.normal_strength,
        min_layer_weight=args.min_layer_weight,
        max_vertices_per_thread=args.max_vertices_per_thread,
        max_workers=args.workers,
    )


def _cmd_generate(args: argparse.Namespace) -> int:
    manifest = generate_project(
        args.out,
        seed=args.seed,
        grid_tiles=(args.grid[0], args.grid[1]),
        tile_size=args.tile_size,
        subdivisions=args.subdivisions,
        points_per_tile=args.points_per_tile,
    )
    _print_json(
        {
            "ok": True,
            "project": str(Path(args.out).resolve()),
            "tile_count": len(manifest["tiles"]),
            "seed": args.seed,
        }
    )
    return 0


def _cmd_bake(args: argparse.Namespace) -> int:
    pipeline = Pipeline(
        args.project,
        args.cache,
        params=_params(args),
        edits_file=args.edits,
    )
    result = pipeline.run(mode=args.mode, fault_tiles=set(args.fail_tile))
    payload = _result_payload(result)
    _print_json(payload)
    return 0 if result.success else 1


def _cmd_edit(args: argparse.Namespace) -> int:
    if args.edits is None:
        raise PipelineError("E_STATE", "edit requires --edits so the operation is recoverable")
    pipeline = Pipeline(
        args.project,
        args.cache,
        params=_params(args),
        edits_file=args.edits,
    )
    if args.height is not None:
        x, y, value = args.height
        report = pipeline.apply_height_edit(args.tile, int(x), int(y), float(value))
    else:
        layer, value = args.material
        report = pipeline.apply_material_edit(args.tile, layer, float(value))
    if args.no_bake:
        _print_json(
            {
                "ok": True,
                "bake": "skipped",
                "invalidations": report.to_dict(),
                "edits": str(Path(args.edits).resolve()),
            }
        )
        return 0
    result = pipeline.run(mode="incremental")
    payload = _result_payload(result)
    payload["edits"] = str(Path(args.edits).resolve())
    _print_json(payload)
    return 0 if result.success else 1


def _cmd_compare(args: argparse.Namespace) -> int:
    result = compare_snapshots(args.left_cache, args.right_cache, raise_on_diff=not args.no_raise)
    _print_json({"ok": bool(result.get("equal")), "comparison": result})
    return 0 if result.get("equal") else 1


def _cmd_inspect(args: argparse.Namespace) -> int:
    snapshot = inspect_cache(args.cache)
    tiles = snapshot.get("tiles", [])
    _print_json(
        {
            "ok": True,
            "format": snapshot.get("format"),
            "project_hash": snapshot.get("project_hash"),
            "bake_version": snapshot.get("bake_version"),
            "tile_count": snapshot.get("tile_count"),
            "tiles": [
                {
                    "tile_id": entry.get("tile_id"),
                    "cache_key": entry.get("cache_key"),
                    "vertex_count": entry.get("vertex_count"),
                    "instance_count": entry.get("instance_count"),
                    "bounds": entry.get("bounds"),
                }
                for entry in tiles
            ],
        }
    )
    return 0


def _cmd_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    edits_file = args.edits
    if edits_file is None:
        edits_file = str(Path(args.cache).resolve().parent / "edits.json")
    run_gui(
        args.project,
        args.cache,
        params=_params(args),
        edits_file=edits_file,
        smoke_test=args.smoke_test,
    )
    return 0


def _result_payload(result: Any) -> dict[str, Any]:
    return {
        "ok": result.success,
        "error": result.error,
        "snapshot": {
            "project_hash": result.snapshot.get("project_hash") if result.snapshot else None,
            "bake_version": result.snapshot.get("bake_version") if result.snapshot else None,
            "tile_count": result.snapshot.get("tile_count") if result.snapshot else None,
        },
        "invalidations": result.invalidations.to_dict(),
        "rebuilt": result.rebuilt,
        "reused": result.reused,
        "recovered": result.recovered,
        "timings_ms": result.timings_ms,
        "total_ms": result.total_ms,
        "effective_workers": result.effective_workers,
    }


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


if __name__ == "__main__":
    raise SystemExit(main())
