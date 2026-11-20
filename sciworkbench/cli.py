"""Scriptable diagnostic entry point."""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path
from typing import Optional

from .engine import Workbench
from .errors import (
    E_BUSY,
    E_CANCELLED,
    E_ERROR_ATTR,
    E_FACTOR_INVALID,
    E_IMG_INVALID,
    E_MODE_INVALID,
    E_RECOVERY_INVALID,
    ForcedTermination,
    WorkbenchError,
)
from .factors import FactorSet, load_factors
from .model import load_sequence
from .sample import PROFILES, SampleProfile, generate_matrix, generate_sample_bundle
from .util import canonical_json_text, write_text_atomic


EXIT_CODES = {
    E_IMG_INVALID: 2,
    E_MODE_INVALID: 3,
    E_BUSY: 4,
    E_ERROR_ATTR: 5,
    E_RECOVERY_INVALID: 6,
    E_FACTOR_INVALID: 7,
    E_CANCELLED: 130,
}


def _json_print(value) -> None:
    sys.stdout.write(canonical_json_text(value))


def _error_print(error: WorkbenchError) -> None:
    sys.stderr.write(canonical_json_text(error.to_dict()))


def _load_workbench(args) -> tuple:
    sequence = load_sequence(args.sequence, block_size=args.block_size)
    if args.calibration:
        factors = load_factors(args.calibration, sequence.structure)
    else:
        factors = FactorSet.defaults(
            sequence.header.width,
            sequence.header.height,
            sequence.structure.block_rows,
            sequence.structure.block_cols,
            frac_bits=args.frac_bits if args.frac_bits is not None else 12,
        )
    if args.frac_bits is not None and factors.frac_bits != args.frac_bits:
        factors = factors.replace("frac_bits", args.frac_bits)
    if args.threshold is not None:
        error_model = factors.error
        factors = factors.replace(
            "error",
            type(error_model)(
                dark_error=error_model.dark_error,
                flat_rel_error=error_model.flat_rel_error,
                flat_floor=error_model.flat_floor,
                threshold=args.threshold,
            ),
        )
    snapshot_root = args.snapshot_root
    if snapshot_root is None:
        snapshot_root = Path(args.sequence).parent / "snapshots"
    workbench = Workbench(
        block_size=args.block_size,
        snapshot_root=snapshot_root,
    )
    workbench.initialize(sequence, factors, args.mode)
    return workbench, sequence, factors


def _cmd_generate(args) -> int:
    if args.matrix:
        names = [item.strip() for item in args.profiles.split(",") if item.strip()]
        profiles = [PROFILES[name] for name in names]
        bundles = generate_matrix(
            args.out,
            profiles=profiles,
            include_mode_comparison=not args.no_mode_comparison,
        )
        _json_print(
            {
                "ok": True,
                "matrix": str(Path(args.out) / "matrix.json"),
                "profiles": [item["profile"]["name"] for item in bundles],
            }
        )
        return 0
    profile = PROFILES[args.preset]
    if args.seed is not None:
        block_size = (
            args.block_size if args.block_size is not None else profile.block_size
        )
        profile = SampleProfile(
            name=profile.name,
            width=args.width if args.width is not None else profile.width,
            height=args.height if args.height is not None else profile.height,
            frames=args.frames if args.frames is not None else profile.frames,
            bad_density=(
                args.bad_density
                if args.bad_density is not None
                else profile.bad_density
            ),
            noise_sigma=(
                args.noise_sigma
                if args.noise_sigma is not None
                else profile.noise_sigma
            ),
            seed=args.seed,
            block_size=block_size,
            frac_bits=args.frac_bits if args.frac_bits is not None else profile.frac_bits,
        )
    elif any(
        value is not None
        for value in (
            args.width,
            args.height,
            args.frames,
            args.bad_density,
            args.noise_sigma,
            args.block_size,
        )
    ):
        block_size = (
            args.block_size if args.block_size is not None else profile.block_size
        )
        profile = SampleProfile(
            name=profile.name,
            width=args.width if args.width is not None else profile.width,
            height=args.height if args.height is not None else profile.height,
            frames=args.frames if args.frames is not None else profile.frames,
            bad_density=(
                args.bad_density
                if args.bad_density is not None
                else profile.bad_density
            ),
            noise_sigma=(
                args.noise_sigma
                if args.noise_sigma is not None
                else profile.noise_sigma
            ),
            seed=profile.seed,
            block_size=block_size,
            frac_bits=args.frac_bits if args.frac_bits is not None else profile.frac_bits,
        )
    bundle = generate_sample_bundle(
        args.out,
        profile,
        write_baseline=not args.no_baseline,
        include_mode_comparison=not args.no_mode_comparison,
    )
    result = bundle.get("result")
    _json_print(
        {
            "ok": True,
            "profile": profile.canonical_dict(),
            "sequence": str(Path(args.out) / "sequence.simg"),
            "calibration": str(Path(args.out) / "factors.cal"),
            "overall_digest": result.overall_digest if result else None,
        }
    )
    return 0


def _cmd_demo(args) -> int:
    names = [item.strip() for item in args.profiles.split(",") if item.strip()]
    profiles = [PROFILES[name] for name in names]
    bundles = generate_matrix(
        args.out,
        profiles=profiles,
        include_mode_comparison=not args.no_mode_comparison,
    )
    _json_print(
        {
            "ok": True,
            "out": str(Path(args.out)),
            "profiles": [
                {
                    "name": item["profile"]["name"],
                    "overall_digest": item["result"].overall_digest,
                    "untrusted_pixels": int(item["result"].untrusted.sum()),
                }
                for item in bundles
            ],
        }
    )
    return 0


def _cmd_inspect(args) -> int:
    sequence = load_sequence(args.sequence, block_size=args.block_size)
    _json_print(
        {
            "ok": True,
            "header": sequence.header.canonical_dict(),
            "sequence_hash": sequence.sequence_hash,
            "frame_digests": [
                sequence.frame_digest(frame)
                for frame in range(sequence.header.frame_count)
            ],
            "structure": sequence.structure.canonical_dict(),
        }
    )
    return 0


def _cmd_process(args) -> int:
    workbench, sequence, factors = _load_workbench(args)
    if getattr(args, "recover", False):
        resume = True
    else:
        resume = args.resume
    try:
        result = workbench.process(
            resume=resume,
            interrupt_after_commits=args.interrupt_after,
        )
    except ForcedTermination as exc:
        sys.stderr.write(
            canonical_json_text(
                {
                    "code": "E_FORCED_TERMINATION",
                    "message": str(exc),
                    "committed_blocks": int(exc.committed_blocks),
                }
            )
        )
        sys.stderr.flush()
        os._exit(137)
    if args.output:
        result.write_outputs(args.output)
    if args.trace:
        write_text_atomic(args.trace, result.trace_jsonl().decode("utf-8"))
    comparison = None
    if args.compare_modes:
        comparison = workbench.compare_modes()
        if args.comparison_output:
            write_text_atomic(
                args.comparison_output, canonical_json_text(comparison)
            )
        else:
            write_text_atomic(
                Path(args.output or ".") / "mode_comparison.json",
                canonical_json_text(comparison),
            )
    _json_print(
        {
            "ok": True,
            "overall_digest": result.overall_digest,
            "frame_digests": list(result.frame_digests),
            "mode": result.mode,
            "snapshot_version": result.snapshot_version,
            "recomputed_blocks": result.recomputed_blocks,
            "reused_blocks": result.reused_blocks,
            "recovered_blocks": result.recovered_blocks,
            "untrusted_pixels": int(result.untrusted.sum()),
            "fixed_float_max_abs": result.fixed_float_comparison["max_abs_diff"],
            "mode_comparison": comparison,
        }
    )
    return 0


def _cmd_compare(args) -> int:
    workbench, _, _ = _load_workbench(args)
    report = workbench.compare_modes()
    if args.output:
        write_text_atomic(args.output, canonical_json_text(report))
    _json_print({"ok": True, "report": report})
    return 0


def _cmd_gui(args) -> int:
    from .gui import run_gui

    return run_gui(
        sequence_path=args.sequence,
        calibration_path=args.calibration,
        snapshot_root=args.snapshot_root,
        block_size=args.block_size,
        demo=args.demo,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m sciworkbench",
        description="Scientific image-sequence diagnostic workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate a deterministic sample")
    generate.add_argument("--out", required=True)
    generate.add_argument("--preset", choices=sorted(PROFILES), default="micro")
    generate.add_argument("--matrix", action="store_true")
    generate.add_argument("--profiles", default="micro,fast,wide")
    generate.add_argument("--width", type=int)
    generate.add_argument("--height", type=int)
    generate.add_argument("--frames", type=int)
    generate.add_argument("--bad-density", type=float)
    generate.add_argument("--noise-sigma", type=float)
    generate.add_argument("--seed", type=int)
    generate.add_argument("--block-size", type=int)
    generate.add_argument("--frac-bits", type=int)
    generate.add_argument("--no-baseline", action="store_true")
    generate.add_argument("--no-mode-comparison", action="store_true")
    generate.set_defaults(func=_cmd_generate)

    demo = subparsers.add_parser("demo", help="generate and diagnose a profile matrix")
    demo.add_argument("--out", required=True)
    demo.add_argument("--profiles", default="micro,fast,wide")
    demo.add_argument("--no-mode-comparison", action="store_true")
    demo.set_defaults(func=_cmd_demo)

    inspect = subparsers.add_parser("inspect", help="validate and describe a sequence")
    inspect.add_argument("--sequence", required=True)
    inspect.add_argument("--block-size", type=int, default=64)
    inspect.set_defaults(func=_cmd_inspect)

    for name in ("process", "recover"):
        command = subparsers.add_parser(name)
        command.add_argument("--sequence", required=True)
        command.add_argument("--calibration")
        command.add_argument("--output")
        command.add_argument("--trace")
        command.add_argument("--snapshot-root")
        command.add_argument("--block-size", type=int, default=64)
        command.add_argument("--mode", choices=("fixed", "float"), default="fixed")
        command.add_argument("--frac-bits", type=int)
        command.add_argument("--threshold", type=float)
        command.add_argument("--resume", action="store_true")
        command.add_argument("--compare-modes", action="store_true")
        command.add_argument("--comparison-output")
        command.add_argument("--interrupt-after", type=int)
        command.set_defaults(
            func=_cmd_process,
            recover=(name == "recover"),
        )

    compare = subparsers.add_parser("compare", help="compare fixed and float modes")
    compare.add_argument("--sequence", required=True)
    compare.add_argument("--calibration")
    compare.add_argument("--output")
    compare.add_argument("--snapshot-root")
    compare.add_argument("--block-size", type=int, default=64)
    compare.add_argument("--mode", default="fixed", choices=("fixed", "float"))
    compare.add_argument("--frac-bits", type=int)
    compare.add_argument("--threshold", type=float)
    compare.set_defaults(func=_cmd_compare)

    gui = subparsers.add_parser("gui", help="launch the PyQt5 diagnostic workbench")
    gui.add_argument("--sequence")
    gui.add_argument("--calibration")
    gui.add_argument("--snapshot-root")
    gui.add_argument("--block-size", type=int, default=64)
    gui.add_argument("--demo", action="store_true")
    gui.set_defaults(func=_cmd_gui)
    return parser


def main(argv=None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        _error_print(exc)
        return EXIT_CODES.get(exc.code, 1)
    except (OSError, ValueError) as exc:
        _error_print(
            WorkbenchError(
                E_IMG_INVALID if isinstance(exc, OSError) else E_FACTOR_INVALID,
                str(exc),
            )
        )
        return 1
