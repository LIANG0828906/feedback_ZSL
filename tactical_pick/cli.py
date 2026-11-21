"""Command line entry points for generation, verification and injection."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Sequence

from .canonical import canonical_json_text
from .errors import PickError
from .generator import build_dataset, verify_dataset, write_dataset
from .injection import INJECTION_ALIASES, run_injection


def _print_json(value: object, *, stream: object = sys.stdout) -> None:
    print(canonical_json_text(value), file=stream)


def _generate(args: argparse.Namespace) -> int:
    if args.inject is not None:
        run_injection(args.inject, seed=args.seed)
        _print_json(
            {
                "ok": False,
                "code": "E_INJECT_UNKNOWN",
                "message": "injection unexpectedly passed",
            },
            stream=sys.stderr,
        )
        return 3
    bundle = build_dataset(args.seed)
    write_dataset(bundle, args.samples, args.baselines)
    summary = {
        "ok": True,
        "seed": args.seed,
        "samples_dir": str(Path(args.samples)),
        "baselines_dir": str(Path(args.baselines)),
        "samples": [
            {
                "sample_id": run.sample_id,
                "final_version": run.final_version,
                "trace_records": len(run.recorder.records),
                "trace_hash": run.recorder.chain_head,
            }
            for run in bundle.runs
        ],
    }
    _print_json(summary)
    return 0


def _verify(args: argparse.Namespace) -> int:
    report = verify_dataset(args.samples, args.baselines)
    _print_json(report)
    return 0


def _inject(args: argparse.Namespace) -> int:
    run_injection(args.kind, seed=args.seed)
    _print_json(
        {
            "ok": False,
            "code": "E_INJECT_UNKNOWN",
            "message": "injection unexpectedly passed",
        },
        stream=sys.stderr,
    )
    return 3


def _gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(args.samples, args.baselines)


def _inspect(args: argparse.Namespace) -> int:
    manifest_path = Path(args.samples) / "manifest.json"
    try:
        manifest = json.loads(manifest_path.read_text("utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise PickError(
            code="E_SCENE_INVALID",
            message="sample manifest could not be read",
            location={"field": "samples.manifest", "error": str(exc)},
        ) from exc
    _print_json(manifest)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m tactical_pick",
        description="Deterministic tactical battlefield picking workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser(
        "generate",
        help="generate deterministic scenes, scenarios and baselines",
    )
    generate.add_argument("--seed", type=int, default=7)
    generate.add_argument("--samples", default="samples")
    generate.add_argument("--baselines", default="baselines")
    generate.add_argument(
        "--inject",
        default=None,
        metavar="KIND",
        help="run an entry-boundary injection before creating any output directory",
    )
    generate.set_defaults(handler=_generate)

    verify = subparsers.add_parser(
        "verify",
        help="regenerate scenarios and compare baselines byte-for-byte",
    )
    verify.add_argument("--samples", default="samples")
    verify.add_argument("--baselines", default="baselines")
    verify.set_defaults(handler=_verify)

    inject = subparsers.add_parser(
        "inject",
        help="exercise an entry-point fault injection without writing results",
    )
    inject.add_argument(
        "--kind",
        required=True,
        choices=sorted(INJECTION_ALIASES),
    )
    inject.add_argument("--seed", type=int, default=7)
    inject.set_defaults(handler=_inject)

    inspect = subparsers.add_parser("inspect", help="print the sample manifest")
    inspect.add_argument("--samples", default="samples")
    inspect.set_defaults(handler=_inspect)

    gui = subparsers.add_parser("gui", help="open the optional PyQt5 workbench")
    gui.add_argument("--samples", default="samples")
    gui.add_argument("--baselines", default="baselines")
    gui.set_defaults(handler=_gui)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except PickError as exc:
        _print_json(exc.to_dict(), stream=sys.stderr)
        return 2
    except KeyboardInterrupt:
        return 130
