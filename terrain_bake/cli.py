"""Command line interface for the terrain baking pipeline."""

import argparse
import json
import sys

from .pipeline import default_params, run_bake
from .verify import compare_bakes


def _params_from_args(args):
    return default_params(
        seed=args.seed, chunk_verts=args.chunk_verts,
        chunks_x=args.chunks_x, chunks_y=args.chunks_y,
        world_size_x=args.world_size, world_size_z=args.world_size,
        height_scale=args.height_scale,
    )


def main(argv=None):
    parser = argparse.ArgumentParser(prog="terrain_bake")
    sub = parser.add_subparsers(dest="command", required=True)

    bake = sub.add_parser("bake", help="run (or resume) a chunked bake")
    bake.add_argument("--out", required=True, help="output directory")
    bake.add_argument("--seed", type=int, default=1337)
    bake.add_argument("--chunk-verts", type=int, default=17)
    bake.add_argument("--chunks-x", type=int, default=4)
    bake.add_argument("--chunks-y", type=int, default=4)
    bake.add_argument("--world-size", type=float, default=256.0)
    bake.add_argument("--height-scale", type=float, default=24.0)
    bake.add_argument("--crash-after", type=int, default=None,
                      help="test hook: hard-exit after N committed chunks")

    verify = sub.add_parser("verify",
                            help="byte-compare two bake output directories")
    verify.add_argument("dir_a")
    verify.add_argument("dir_b")

    args = parser.parse_args(argv)
    if args.command == "bake":
        report = run_bake(_params_from_args(args), args.out,
                          crash_after=args.crash_after)
        print(json.dumps(report, indent=2))
        return 0
    if args.command == "verify":
        result = compare_bakes(args.dir_a, args.dir_b)
        print(json.dumps(result, indent=2))
        return 0 if result["ok"] else 1
    return 2


if __name__ == "__main__":
    sys.exit(main())
