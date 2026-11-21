"""Command-line entry points."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from .engine import (
    ReplayEngine,
    compute_quantization_impact,
    scenario_fingerprint,
)
from .errors import (
    E_CANCELLED,
    E_NO_DISPLAY,
    E_REPLAY_INVALID,
    ReplayError,
)
from .generator import generate_matrix, generate_sample
from .loader import load_scenario
from .snapshot import atomic_write_bytes, canonical_json_bytes


def _add_generate_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--output", required=True, help="sample output directory")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--covers", type=int, default=8)
    parser.add_argument("--players", type=int, default=3)
    parser.add_argument("--revoke-density", type=float, default=0.2)
    parser.add_argument("--steps", type=int, default=24)
    parser.add_argument("--parallelism", type=int, default=2)
    parser.add_argument("--memory-budget", type=int, default=16384)
    parser.add_argument("--clean", action="store_true")


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="cover-replay",
        description="Deterministic tactical cover replay and baseline verifier",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser(
        "generate",
        help="generate one deterministic local scenario and baseline",
    )
    _add_generate_arguments(generate_parser)

    matrix_parser = subparsers.add_parser(
        "generate-matrix",
        help="generate several deterministic scenario combinations",
    )
    matrix_parser.add_argument("--output", required=True)
    matrix_parser.add_argument("--seed", type=int, default=42)
    matrix_parser.add_argument("--steps", type=int, default=20)
    matrix_parser.add_argument("--clean", action="store_true")

    replay_parser = subparsers.add_parser(
        "replay",
        help="replay a scenario, publish snapshots, and compare a baseline",
    )
    replay_parser.add_argument("--input", required=True, help="scenario directory or match JSON")
    replay_parser.add_argument("--output", required=True, help="fresh replay output directory")
    replay_parser.add_argument("--clean", action="store_true")
    replay_parser.add_argument("--resume", action="store_true")
    replay_parser.add_argument("--no-baseline", action="store_true")
    replay_parser.add_argument("--decimals", type=int)
    replay_parser.add_argument(
        "--inject-revoke",
        action="append",
        metavar="PLAYER:CLIENT_SEQ[:STEP]",
        help="inject a revoke before replay starts; repeatable",
    )
    replay_parser.add_argument("--max-steps", type=int)
    replay_parser.add_argument(
        "--crash-after-step",
        type=int,
        help="test hook: force-terminate once this physical step is snapshotted",
    )
    replay_parser.add_argument("--json", action="store_true")

    gui_parser = subparsers.add_parser("gui", help="launch the PySide6 desktop interface")
    gui_parser.add_argument("--input")
    gui_parser.add_argument("--output")
    gui_parser.add_argument("--resume", action="store_true")
    gui_parser.add_argument("--step-delay-ms", type=int, default=120)

    inspect_parser = subparsers.add_parser(
        "fingerprint",
        help="print the canonical scenario fingerprint",
    )
    inspect_parser.add_argument("--input", required=True)
    return parser


def _print_error(error: ReplayError, as_json: bool) -> None:
    if as_json:
        print(json.dumps(error.to_dict(), ensure_ascii=False, sort_keys=True))
    else:
        print(str(error), file=sys.stderr)
        if error.details:
            print(
                json.dumps(error.details, ensure_ascii=False, sort_keys=True, indent=2),
                file=sys.stderr,
            )


def _run_generate(args: argparse.Namespace) -> int:
    manifest = generate_sample(
        args.output,
        seed=args.seed,
        cover_count=args.covers,
        player_count=args.players,
        revoke_density=args.revoke_density,
        steps=args.steps,
        parallelism=args.parallelism,
        memory_budget_bytes=args.memory_budget,
        clean=args.clean,
    )
    print(json.dumps(manifest, ensure_ascii=False, sort_keys=True, indent=2))
    return 0


def _run_matrix(args: argparse.Namespace) -> int:
    manifests = generate_matrix(
        args.output,
        seed=args.seed,
        steps=args.steps,
        clean=args.clean,
    )
    print(
        json.dumps(
            {
                "output": str(Path(args.output).resolve()),
                "cases": len(manifests),
                "case_names": [item["case"] for item in manifests],
            },
            ensure_ascii=False,
            sort_keys=True,
            indent=2,
        )
    )
    return 0


def _run_replay(args: argparse.Namespace) -> int:
    scenario = load_scenario(args.input)
    engine = ReplayEngine(
        scenario,
        args.output,
        clean=args.clean,
        resume=args.resume,
        compare_baseline=not args.no_baseline,
        decimals_override=args.decimals,
        crash_after_step=args.crash_after_step,
    )
    injected: list[dict[str, Any]] = []
    for specification in args.inject_revoke or []:
        parts = specification.split(":")
        if len(parts) not in {2, 3}:
            raise ReplayError(
                E_REPLAY_INVALID,
                "injected revoke must use PLAYER:CLIENT_SEQ[:STEP]",
                None,
                "inject_revoke",
                {"actual": specification},
            )
        player_id = parts[0]
        try:
            client_seq = int(parts[1])
            effective_step = int(parts[2]) if len(parts) == 3 else None
        except ValueError:
            raise ReplayError(
                E_REPLAY_INVALID,
                "injected revoke sequence and step must be integers",
                None,
                "inject_revoke",
                {"actual": specification},
            ) from None
        if effective_step is None:
            shot = scenario.shot_by_key.get(f"{player_id}:{client_seq}")
            selected_step = (
                max(engine.next_step, shot.effective_step)
                if shot is not None
                else engine.next_step
            )
        else:
            selected_step = effective_step
        revoke = engine.inject_revoke(
            player_id,
            client_seq,
            effective_step=selected_step,
        )
        injected.append(
            {
                "player_id": revoke.player_id,
                "client_seq": revoke.client_seq,
                "effective_step": revoke.effective_step,
                "arrival_seq": revoke.arrival_seq,
            }
        )
    result = engine.run(max_steps=args.max_steps)
    quantization_impact: dict[str, Any] | None = None
    if args.decimals is not None and args.decimals != scenario.config.fixed_point_decimals:
        quantization_impact = compute_quantization_impact(
            scenario,
            scenario.config.fixed_point_decimals,
            args.decimals,
        )
        atomic_write_bytes(
            engine.output_dir / "quantization_impact.json",
            canonical_json_bytes(quantization_impact) + b"\n",
        )
        result.quantization_impact = quantization_impact
    summary = result.to_dict()
    summary["output"] = str(engine.output_dir)
    summary["scenario_fingerprint"] = engine.fingerprint
    summary["injected_revokes"] = injected
    summary["quantization_impact_path"] = (
        str(engine.output_dir / "quantization_impact.json")
        if quantization_impact is not None
        else None
    )
    if args.json:
        print(json.dumps(summary, ensure_ascii=False, sort_keys=True))
    else:
        print(
            f"status={summary['status']} "
            f"steps={summary['steps_completed']} "
            f"last_step={summary['last_step']} "
            f"output={summary['output']}"
        )
        if sensing := summary["baseline_diff"]:
            print(
                f"first baseline mismatch at step {sensing['step']} "
                f"field {sensing['field_path']}",
                file=sys.stderr,
            )
        if quantization_impact is not None:
            print(
                f"quantization affected covers={quantization_impact['affected_cover_count']} "
                f"report={summary['quantization_impact_path']}"
            )
    if result.status == "baseline_mismatch":
        return 2
    if result.status == "cancelled":
        return 130
    return 0


def _run_fingerprint(args: argparse.Namespace) -> int:
    scenario = load_scenario(args.input)
    print(scenario_fingerprint(scenario))
    return 0


def _run_gui(args: argparse.Namespace) -> int:
    if (
        sys.platform.startswith("linux")
        and not os.environ.get("DISPLAY")
        and not os.environ.get("WAYLAND_DISPLAY")
        and os.environ.get("QT_QPA_PLATFORM") not in {"offscreen", "minimal"}
    ):
        error = ReplayError(
            E_NO_DISPLAY,
            "no display server detected; use the replay CLI in headless mode",
            None,
            "gui",
        )
        _print_error(error, False)
        return 2
    try:
        from .gui import launch_gui
    except ImportError as error:
        replay_error = ReplayError(
            E_NO_DISPLAY,
            f"PySide6 GUI dependencies are unavailable: {error}",
            None,
            "gui",
        )
        _print_error(replay_error, False)
        return 2
    return launch_gui(
        input_path=args.input,
        output_path=args.output,
        resume=args.resume,
        step_delay_ms=args.step_delay_ms,
    )


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            return _run_generate(args)
        if args.command == "generate-matrix":
            return _run_matrix(args)
        if args.command == "replay":
            return _run_replay(args)
        if args.command == "fingerprint":
            return _run_fingerprint(args)
        if args.command == "gui":
            return _run_gui(args)
    except ReplayError as error:
        _print_error(error, getattr(args, "json", False))
        if error.code == E_CANCELLED:
            return 130
        return 2
    except KeyboardInterrupt:
        print("cancelled", file=sys.stderr)
        return 130
    parser.error("unknown command")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())

