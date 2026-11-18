"""Command line entry points for scene generation, validation and rendering."""

from __future__ import annotations

import argparse
import json
import os
import sys

from .band_report import write_band_report
from .compare import compare_runs
from .io_utils import write_json
from .render import GracefulInterrupt, Renderer
from .scene import build_default_scene, load_scene_dict, parse_scene, write_scene
from .validate import SceneValidationError, assert_valid

EXIT_OK = 0
EXIT_VALIDATION = 2
EXIT_MISMATCH = 3
EXIT_RUNTIME = 4


def _emit_validation(error: SceneValidationError, path: str) -> None:
    payload = {
        "status": "validation_failed",
        "error_count": len(error.issues),
        "issues": [issue.as_dict() for issue in error.issues],
    }
    if path:
        write_json(path, payload)
    json.dump(payload, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")


def cmd_generate(args: argparse.Namespace) -> int:
    description = build_default_scene()
    write_scene(description, args.scene)
    print(f"scene written to {args.scene}")
    return EXIT_OK


def cmd_validate(args: argparse.Namespace) -> int:
    description = load_scene_dict(args.scene)
    try:
        assert_valid(description)
    except SceneValidationError as error:
        _emit_validation(error, args.report)
        return EXIT_VALIDATION
    print("scene validation OK")
    return EXIT_OK


def cmd_render(args: argparse.Namespace) -> int:
    description = load_scene_dict(args.scene)
    try:
        assert_valid(description)
    except SceneValidationError as error:
        _emit_validation(error, args.error_report)
        return EXIT_VALIDATION
    scene = parse_scene(description)
    renderer = Renderer(scene, args.output)
    renderer.interrupt_after_tiles = args.interrupt_after_tiles
    if args.handle_sigint:
        renderer.install_sigint()
    try:
        renderer.run()
    except GracefulInterrupt:
        print("graceful interrupt: tile committed, recovery report written")
        return EXIT_OK
    except RuntimeError as error:
        write_json(
            os.path.join(args.output, "runtime_error.json"),
            {"status": "runtime_error", "message": str(error)},
        )
        return EXIT_RUNTIME
    finally:
        if args.handle_sigint:
            renderer.restore_sigint()
    write_band_report(scene, args.output)
    print(f"render complete: {args.output}")
    return EXIT_OK


def cmd_compare(args: argparse.Namespace) -> int:
    differences = compare_runs(args.reference, args.candidate, args.scene)
    payload = {
        "status": "identical" if not differences else "mismatch",
        "difference_count": len(differences),
        "differences": differences,
    }
    write_json(args.report, payload)
    if differences:
        json.dump(payload, sys.stdout, indent=2, sort_keys=True)
        sys.stdout.write("\n")
        return EXIT_MISMATCH
    print("byte-identical render outputs")
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="kiln-glass",
        description="Deterministic glass-brick kiln observation window renderer",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="deterministically build the scene JSON")
    gen.add_argument("--scene", required=True)
    gen.set_defaults(func=cmd_generate)

    val = sub.add_parser("validate", help="validate a scene JSON")
    val.add_argument("--scene", required=True)
    val.add_argument("--report", default="")
    val.set_defaults(func=cmd_validate)

    ren = sub.add_parser("render", help="render all views with tile snapshots")
    ren.add_argument("--scene", required=True)
    ren.add_argument("--output", required=True)
    ren.add_argument("--interrupt-after-tiles", type=int, default=None)
    ren.add_argument("--handle-sigint", action="store_true")
    ren.add_argument("--error-report", default="")
    ren.set_defaults(func=cmd_render)

    cmp_cmd = sub.add_parser("compare", help="structured diff of two output trees")
    cmp_cmd.add_argument("--reference", required=True)
    cmp_cmd.add_argument("--candidate", required=True)
    cmp_cmd.add_argument("--report", required=True)
    cmp_cmd.add_argument("--scene", default=None,
                         help="optional scene JSON for accurate tile ids")
    cmp_cmd.set_defaults(func=cmd_compare)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
