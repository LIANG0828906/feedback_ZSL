"""Command line interface.

Usage:
    python -m willow_renderer gen-sample --out scene.json [--seed N]
    python -m willow_renderer render --scene scene.json --outdir out [options]

Exit codes:
    0  success (including graceful interrupt after committing the tile)
    1  unexpected internal error
    2  scene validation error (structured code on stderr)
    3  scene IO / parse error
    4  verification mismatch against reference outputs
    5  corrupt snapshot in --strict-snapshot mode
"""

import argparse
import json
import os
import sys

from . import driver, sample_scene
from .errors import EXIT_OK, EXIT_UNEXPECTED, RendererError
from .scene import load_scene


def _cmd_gen_sample(args):
    sample_scene.write_sample_scene(
        args.out, seed=args.seed, image_size=args.image_size,
        tile_size=args.tile_size)
    print(f"sample scene written to {args.out}")
    return EXIT_OK


def _cmd_render(args):
    scene = load_scene(args.scene)
    tile_delay = int(os.environ.get("WILLOW_TILE_DELAY_MS", "0"))
    result = driver.render(
        scene, args.outdir,
        strict_snapshot=args.strict_snapshot,
        expect_rgba=args.expect_rgba,
        expect_owner=args.expect_owner,
        expect_landings=args.expect_landings,
        tile_delay_ms=tile_delay,
    )
    print(json.dumps({"status": "interrupted" if result["interrupted"] else "completed",
                      "completed_tiles": result["completed_tiles"],
                      "total_tiles": result["total_tiles"],
                      "resumed_from_tiles": result["resumed_from_tiles"],
                      "discarded_snapshot_reason":
                          result["discarded_snapshot_reason"]},
                     ensure_ascii=False))
    return EXIT_OK


def main(argv=None):
    parser = argparse.ArgumentParser(prog="willow_renderer")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("gen-sample", help="generate a deterministic sample scene")
    gen.add_argument("--out", required=True)
    gen.add_argument("--seed", type=int, default=20261118)
    gen.add_argument("--image-size", type=int, default=256)
    gen.add_argument("--tile-size", type=int, default=64)
    gen.set_defaults(func=_cmd_gen_sample)

    ren = sub.add_parser("render", help="render a scene to RGBA + owner map + report")
    ren.add_argument("--scene", required=True)
    ren.add_argument("--outdir", required=True)
    ren.add_argument("--strict-snapshot", action="store_true",
                     help="exit with code 5 when a persisted snapshot is corrupt "
                          "(default: discard and recompute from scratch)")
    ren.add_argument("--expect-rgba", default=None,
                     help="reference RGBA PNG for byte-exact verification")
    ren.add_argument("--expect-owner", default=None,
                     help="reference owner-map PNG for byte-exact verification")
    ren.add_argument("--expect-landings", default=None,
                     help="reference landings JSON for verification")
    ren.set_defaults(func=_cmd_render)

    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except RendererError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False), file=sys.stderr)
        return exc.exit_code
    except Exception as exc:  # noqa: BLE001 - top-level guard
        payload = {"error": {"code": "INTERNAL_ERROR", "message": repr(exc),
                             "layer_id": None, "field": None}}
        print(json.dumps(payload, ensure_ascii=False), file=sys.stderr)
        return EXIT_UNEXPECTED


if __name__ == "__main__":
    sys.exit(main())
