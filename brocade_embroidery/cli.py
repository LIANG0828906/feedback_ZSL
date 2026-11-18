"""Command line entry points for generation, validation and rendering."""

from __future__ import annotations

import argparse
import json
import os
import sys

import numpy as np
from PIL import Image

from .render import render_frame
from .acceptance import run_acceptance
from .scene_gen import generate_scene, scene_canonical_json
from .validation import SceneError, validate_scene


def _load_scene(path: str) -> dict:
    with open(path, "rb") as handle:
        return json.loads(handle.read().decode("utf-8"))


def _write_json(path: str, payload) -> None:
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, sort_keys=True, indent=2,
                  ensure_ascii=False)


def cmd_generate(args: argparse.Namespace) -> int:
    scene = generate_scene(small=args.small, seed=args.seed)
    os.makedirs(os.path.dirname(args.scene) or ".", exist_ok=True)
    with open(args.scene, "wb") as handle:
        handle.write(scene_canonical_json(scene))
        handle.write(b"\n")
    print(f"scene written: {args.scene}")
    return 0


def cmd_validate(args: argparse.Namespace) -> int:
    scene = _load_scene(args.scene)
    warnings = validate_scene(scene)
    if args.report:
        _write_json(args.report, {"valid": not warnings, "warnings": warnings})
    if warnings:
        print(json.dumps({"valid": False, "warnings": warnings},
                         ensure_ascii=False, indent=2))
        return 2
    print(json.dumps({"valid": True, "warnings": []}, ensure_ascii=False))
    return 0


def cmd_render(args: argparse.Namespace) -> int:
    scene = _load_scene(args.scene)
    warnings = validate_scene(scene)
    if warnings:
        print(json.dumps({"valid": False, "warnings": warnings},
                         ensure_ascii=False, indent=2))
        return 2
    azimuths = scene["camera"]["orbit_azimuth_deg"]
    if args.frame is not None:
        if not 0 <= args.frame < len(azimuths):
            print(f"frame index {args.frame} out of range", file=sys.stderr)
            return 3
        azimuths = [azimuths[args.frame]]
    exit_code = 0
    for frame_index, azimuth in enumerate(azimuths):
        artifacts = render_frame(
            scene, args.output, frame_index if args.frame is None
            else args.frame, float(azimuth),
            allow_resume=not args.fresh,
            max_tiles=args.max_tiles)
        if artifacts.report.get("incomplete"):
            exit_code = 0 if args.allow_interrupt else 4
    return exit_code


def cmd_compare(args: argparse.Namespace) -> int:
    image_a = np.asarray(Image.open(args.a), dtype=np.uint8)
    image_b = np.asarray(Image.open(args.b), dtype=np.uint8)
    if image_a.shape != image_b.shape:
        payload = [{"error_code": "E-IMAGE-SHAPE",
                    "shape_a": list(image_a.shape),
                    "shape_b": list(image_b.shape)}]
    else:
        mismatch = np.argwhere(np.any(image_a != image_b, axis=-1))
        channels = ["R", "G", "B", "A"][: image_a.shape[-1]]
        payload = [
            {
                "error_code": "E-IMAGE-BYTE-DIFF",
                "pixel_xy": [int(x), int(y)],
                "channels": [c for c in channels
                             if image_a[y, x][channels.index(c)]
                             != image_b[y, x][channels.index(c)]],
                "values_a": [int(v) for v in image_a[y, x]],
                "values_b": [int(v) for v in image_b[y, x]],
            }
            for y, x in mismatch[:50]
        ]
    _write_json(args.report, {"identical": not payload, "differences": payload})
    if payload:
        print(json.dumps({"identical": False, "difference_count": len(payload)},
                         ensure_ascii=False))
        return 5
    print(json.dumps({"identical": True}, ensure_ascii=False))
    return 0


def cmd_acceptance(args: argparse.Namespace) -> int:
    report = run_acceptance(args.output, small=args.small)
    ok = (report["resume_verification"]["png_bytes_identical"]
          and report["shadow_distribution"]["byte_identical"]
          and report["isolated_pixel_count"] == 0)
    print(json.dumps({"acceptance_passed": ok,
                      "report": os.path.join(args.output,
                                             "acceptance_report.json")},
                     ensure_ascii=False))
    return 0 if ok else 6


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="brocade-embroidery",
        description="Deterministic embroidered-stage off-screen renderer")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate-scene", help="create deterministic JSON")
    generate.add_argument("--scene", required=True)
    generate.add_argument("--small", action="store_true")
    generate.add_argument("--seed", type=int, default=20260101)
    generate.set_defaults(func=cmd_generate)

    validate = sub.add_parser("validate-scene", help="validate a scene JSON")
    validate.add_argument("--scene", required=True)
    validate.add_argument("--report")
    validate.set_defaults(func=cmd_validate)

    render = sub.add_parser("render", help="render orbit frames")
    render.add_argument("--scene", required=True)
    render.add_argument("--output", required=True)
    render.add_argument("--frame", type=int)
    render.add_argument("--fresh", action="store_true",
                        help="ignore existing snapshots and start at block zero")
    render.add_argument("--max-tiles", type=int,
                        help="stop after this many new tiles (test hook)")
    render.add_argument("--allow-interrupt", action="store_true")
    render.set_defaults(func=cmd_render)

    compare = sub.add_parser("compare-images", help="byte-level RGBA comparison")
    compare.add_argument("--a", required=True)
    compare.add_argument("--b", required=True)
    compare.add_argument("--report", required=True)
    compare.set_defaults(func=cmd_compare)

    acceptance = sub.add_parser("acceptance", help="run full acceptance checks")
    acceptance.add_argument("--output", required=True)
    acceptance.add_argument("--small", action="store_true")
    acceptance.set_defaults(func=cmd_acceptance)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except SceneError as exc:
        print(json.dumps({"valid": False, "warnings": exc.warnings},
                         ensure_ascii=False, indent=2))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
