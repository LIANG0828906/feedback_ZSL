"""Command-line entry points for generation, rebuild, render and diagnostics."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Optional, Sequence

from .codes import AcousticError, E_CONFIG_INVALID
from .diagnostics import run_diagnostics
from .engine import Workbench
from .injection import INJECTION_CODES, Injection
from .samplegen import generate_samples
from .scene import STAGE_ORDER
from .util import canonical_json_text


def _print_json(value: Any) -> None:
    print(canonical_json_text(value))


def _injections(values: Optional[Sequence[str]]) -> list[Injection]:
    return [Injection.parse(value) for value in values or []]


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="acoustic-workbench",
        description="Deterministic 5.1 spatial acoustic field reconstruction",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sample_parser = subparsers.add_parser("sample", help="Generate local deterministic samples")
    sample_parser.add_argument("--workspace", required=True)
    sample_parser.add_argument("--seed", type=int, default=20260101)
    sample_parser.add_argument(
        "--venues",
        nargs="+",
        default=["small", "medium", "large"],
        choices=["small", "medium", "large"],
    )
    sample_parser.add_argument(
        "--no-baselines",
        action="store_true",
        help="Skip baseline field reconstruction",
    )

    rebuild_parser = subparsers.add_parser("rebuild", help="Rebuild one listener field")
    rebuild_parser.add_argument("--workspace", required=True)
    rebuild_parser.add_argument("--scene", default="scene.json")
    rebuild_parser.add_argument("--listener", required=True)
    rebuild_parser.add_argument(
        "--inject",
        action="append",
        choices=INJECTION_CODES,
        default=[],
    )
    rebuild_parser.add_argument(
        "--cancel-after-stage",
        choices=STAGE_ORDER,
        default=None,
    )
    rebuild_parser.add_argument(
        "--summary",
        action="store_true",
        help="Print a compact rebuild result instead of the full field document",
    )

    render_parser = subparsers.add_parser("render", help="Render a listener field to a 5.1 WAV")
    render_parser.add_argument("--workspace", required=True)
    render_parser.add_argument("--scene", default="scene.json")
    render_parser.add_argument("--listener", required=True)
    render_parser.add_argument("--output", default=None)
    render_parser.add_argument(
        "--inject",
        action="append",
        choices=INJECTION_CODES,
        default=[],
    )

    diagnose_parser = subparsers.add_parser(
        "diagnose", help="Run deterministic acceptance diagnostics"
    )
    diagnose_parser.add_argument("--workspace", required=True)
    diagnose_parser.add_argument("--seed", type=int, default=20260101)

    status_parser = subparsers.add_parser("status", help="Read published versions from disk")
    status_parser.add_argument("--workspace", required=True)
    status_parser.add_argument("--scene", default="scene.json")

    gui_parser = subparsers.add_parser("gui", help="Launch the PyQt5 workbench")
    gui_parser.add_argument("--workspace", required=True)
    gui_parser.add_argument("--scene", default=None)
    return parser


def _run(args: argparse.Namespace) -> int:
    if args.command == "sample":
        manifest = generate_samples(
            args.workspace,
            seed=args.seed,
            venue_names=tuple(args.venues),
            build_baselines=not args.no_baselines,
        )
        _print_json(manifest)
        return 0

    if args.command == "rebuild":
        workbench = Workbench(args.workspace, scene_path=args.scene)
        result = workbench.rebuild(
            args.listener,
            injections=_injections(args.inject),
            cancel_after_stage=args.cancel_after_stage,
        )
        if args.summary:
            trajectory = result["trajectory"]
            _print_json(
                {
                    "ok": True,
                    "field_version": result["field_version"],
                    "trajectory": trajectory,
                }
            )
        else:
            _print_json(result)
        return 0

    if args.command == "render":
        workbench = Workbench(args.workspace, scene_path=args.scene)
        result = workbench.render(
            args.listener,
            output=args.output,
            injections=_injections(args.inject),
        )
        _print_json(result)
        return 0

    if args.command == "diagnose":
        report = run_diagnostics(args.workspace, seed=args.seed)
        _print_json(report)
        return 0 if report["ok"] else 1

    if args.command == "status":
        workbench = Workbench(args.workspace, scene_path=args.scene)
        _print_json(workbench.status())
        return 0

    if args.command == "gui":
        from .gui import launch_gui

        launch_gui(args.workspace, scene_path=args.scene)
        return 0

    raise AcousticError(
        E_CONFIG_INVALID,
        f"Unsupported command: {args.command}",
        injection_point="cli:command",
    )


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        return _run(args)
    except AcousticError as exc:
        _print_json(exc.to_dict())
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

